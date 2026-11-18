package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// ErrDependencyCycle is returned when a new link would close a cycle. It
// matches domain.ErrValidation.
var ErrDependencyCycle = fmt.Errorf("%w: linking these tasks would create a dependency cycle", domain.ErrValidation)

// AddDependency records that taskID cannot start until dependsOnID is
// completed or archived, and applies the effect on taskID (section 4.3):
//
//   - a todo task receives a dependency block, as a quiet system change;
//   - an in_progress task keeps its status and gets a system comment and
//     task.dependency_added, so the assignee decides;
//   - any other task, or a link to a blocker that is already resolved,
//     changes nothing but the link.
//
// Adding a link that already exists is a no-op and returns the task as it is.
//
// Concurrency: dependency inserts are serialised per tenant by an advisory
// lock, so two requests can never each pass the cycle check and together close
// a cycle. Task rows are then locked blocker first, then dependent: the same
// order in which completing a blocker locks its dependents, so the two paths
// cannot deadlock each other.
func (s *TaskService) AddDependency(ctx context.Context, tenantID string, taskID, dependsOnID uuid.UUID) (*domain.Task, error) {
	if taskID == dependsOnID {
		return nil, fmt.Errorf("%w: a task cannot depend on itself", domain.ErrValidation)
	}
	now := s.clock()
	var (
		task   *domain.Task
		events []domain.Event
	)
	err := s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.deps.Dependencies.LockTenant(ctx, tenantID); err != nil {
			return err
		}
		// Both loads are tenant-scoped: a task in another tenant is not found.
		blocker, err := s.deps.Tasks.GetForUpdate(ctx, tenantID, dependsOnID)
		if err != nil {
			return err
		}
		if task, err = s.deps.Tasks.GetForUpdate(ctx, tenantID, taskID); err != nil {
			return err
		}
		dep, err := domain.NewDependency(task, blocker, now)
		if err != nil {
			return err
		}
		cycle, err := s.deps.Dependencies.Reaches(ctx, tenantID, dependsOnID, taskID)
		if err != nil {
			return err
		}
		if cycle {
			return ErrDependencyCycle
		}
		created, err := s.deps.Dependencies.Create(ctx, dep)
		if err != nil || !created {
			return err
		}
		if blocker.ResolvesDependencies() {
			return nil
		}
		events, err = s.applyOpenDependency(ctx, task, blocker, now, domain.EventTaskDependencyAdded,
			"This task now depends on %s, which is not done yet. Its status was left unchanged.")
		return err
	})
	if err != nil {
		return nil, err
	}
	s.signal(events)
	return task, nil
}

// applyOpenDependency handles a dependent whose blocker has just become an
// open dependency, by a new link or by the blocker reopening. A todo task is
// dependency-blocked; a task already started (and, for a reopened blocker, a
// completed one) keeps its status and gets a system comment and noticeType.
// It writes the dependent and the outbox rows, and returns the events.
func (s *TaskService) applyOpenDependency(ctx context.Context, task, blocker *domain.Task, now time.Time, noticeType, comment string) ([]domain.Event, error) {
	var events []domain.Event
	switch {
	case task.Status == domain.StatusTodo:
		before := task.Version
		var err error
		if events, err = task.ApplyDependencyBlock(blocker, now); err != nil {
			return nil, err
		}
		if err := s.deps.Tasks.Update(ctx, task, before); err != nil {
			return nil, err
		}
	case task.Status == domain.StatusInProgress,
		task.Status == domain.StatusCompleted && noticeType == domain.EventTaskDependencyReopened:
		if err := s.systemComment(ctx, task, fmt.Sprintf(comment, describe(blocker)), now); err != nil {
			return nil, err
		}
		events = []domain.Event{domain.NewDependencyEvent(noticeType, task, blocker, now)}
	default:
		return nil, nil
	}
	return events, s.emit(ctx, events, now)
}

// onBlockerStatusChange re-checks blocker's dependents after its status moved
// from `from` to its current status, inside the same transaction (section
// 4.3, decision 17). It returns the events written for the dependents.
func (s *TaskService) onBlockerStatusChange(ctx context.Context, blocker *domain.Task, from domain.Status, now time.Time) ([]domain.Event, error) {
	wasResolved := from == domain.StatusCompleted || from == domain.StatusArchived
	switch {
	case blocker.ResolvesDependencies() && !wasResolved:
		return s.resolveDependents(ctx, blocker, now)
	case !blocker.ResolvesDependencies() && wasResolved:
		return s.reopenDependents(ctx, blocker, now)
	}
	return nil, nil
}

// resolveDependents handles the dependents of a blocker that is now resolved,
// or has been deleted. Dependents are locked in ID order after the blocker.
// For each dependent with no open blocker left:
//
//   - a dependency block is lifted, with task.unblocked;
//   - a manual block stays, with a system comment and task.dependency_resolved;
//   - an open, unblocked task gets task.dependency_resolved.
//
// A completed or archived dependent is not told: the news cannot affect it.
func (s *TaskService) resolveDependents(ctx context.Context, blocker *domain.Task, now time.Time) ([]domain.Event, error) {
	ids, err := s.deps.Dependencies.ListDependents(ctx, blocker.TenantID, blocker.ID)
	if err != nil {
		return nil, err
	}
	return s.resolveEach(ctx, blocker, ids, now)
}

func (s *TaskService) resolveEach(ctx context.Context, blocker *domain.Task, ids []uuid.UUID, now time.Time) ([]domain.Event, error) {
	var all []domain.Event
	for _, id := range ids {
		dep, err := s.deps.Tasks.GetForUpdate(ctx, blocker.TenantID, id)
		if err != nil {
			return nil, err
		}
		open, err := s.deps.Dependencies.HasOpenBlocker(ctx, dep.TenantID, dep.ID)
		if err != nil {
			return nil, err
		}
		if open || !dep.IsOpen() {
			continue
		}
		var events []domain.Event
		switch {
		case dep.Status == domain.StatusBlocked && *dep.BlockedReason == domain.BlockedDependency:
			before := dep.Version
			if events, err = dep.LiftDependencyBlock(blocker, now); err != nil {
				return nil, err
			}
			if err := s.deps.Tasks.Update(ctx, dep, before); err != nil {
				return nil, err
			}
		case dep.Status == domain.StatusBlocked:
			body := fmt.Sprintf("Dependency %s is %s. This task stays blocked because it was blocked manually.", describe(blocker), blocker.Status)
			if err := s.systemComment(ctx, dep, body, now); err != nil {
				return nil, err
			}
			events = []domain.Event{domain.NewDependencyEvent(domain.EventTaskDependencyResolved, dep, blocker, now)}
		default:
			events = []domain.Event{domain.NewDependencyEvent(domain.EventTaskDependencyResolved, dep, blocker, now)}
		}
		if err := s.emit(ctx, events, now); err != nil {
			return nil, err
		}
		all = append(all, events...)
	}
	return all, nil
}

// reopenDependents handles the dependents of a blocker that was restored or
// reopened, so its links count as open again.
func (s *TaskService) reopenDependents(ctx context.Context, blocker *domain.Task, now time.Time) ([]domain.Event, error) {
	ids, err := s.deps.Dependencies.ListDependents(ctx, blocker.TenantID, blocker.ID)
	if err != nil {
		return nil, err
	}
	var all []domain.Event
	for _, id := range ids {
		dep, err := s.deps.Tasks.GetForUpdate(ctx, blocker.TenantID, id)
		if err != nil {
			return nil, err
		}
		events, err := s.applyOpenDependency(ctx, dep, blocker, now, domain.EventTaskDependencyReopened,
			"Dependency %s is open again. This task's status was left unchanged.")
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
	}
	return all, nil
}

func (s *TaskService) systemComment(ctx context.Context, task *domain.Task, body string, now time.Time) error {
	c, err := domain.NewSystemComment(task, body, now)
	if err != nil {
		return err
	}
	return s.deps.Comments.Create(ctx, c)
}

// describe names a task in a system comment.
func describe(t *domain.Task) string {
	return fmt.Sprintf("%q (#%s)", t.Title, t.ID.String()[:8])
}
