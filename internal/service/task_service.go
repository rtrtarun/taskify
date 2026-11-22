// Package service holds Taskify's use cases. Each mutation runs in one
// transaction that writes the task and the outbox rows for its events, so a
// change and its notifications commit together or not at all. No method here
// makes a network call.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

const (
	// DefaultListLimit and MaxListLimit bound a page of tasks.
	DefaultListLimit = 50
	MaxListLimit     = 200
	// recentComments is how many comments GetTask returns.
	recentComments = 20
)

// Deps are the repositories a TaskService uses.
type Deps struct {
	Tx         repository.TxManager
	Tasks      repository.TaskRepository
	Workspaces repository.WorkspaceRepository
	Comments   repository.CommentRepository
	Outbox     repository.OutboxRepository
	// Dependencies is required from R2: task status changes re-check
	// dependents.
	Dependencies repository.DependencyRepository
	// AgentJobs is required from R2: a person's change queues agent jobs in
	// the same transaction.
	AgentJobs repository.AgentJobRepository
}

// Config holds the settings that shape outbox rows.
type Config struct {
	PublicBaseURL string
	// AppBaseURL is the web app's address (APP_BASE_URL), or empty: with
	// it, notifications link to the task's page there (AP-10).
	AppBaseURL        string
	OutboxMaxAttempts int
	// DigestLoadThreshold is how many in_progress p1_critical tasks make an
	// assignee overloaded, so that a non-critical assignment is held for
	// their digest (DIGEST_LOAD_THRESHOLD, default 3, G14).
	DigestLoadThreshold int
}

// links are the addresses notifications link to.
func (c Config) links() domain.Links {
	return domain.Links{API: c.PublicBaseURL, App: c.AppBaseURL}
}

// DefaultDigestLoadThreshold is the default of Config.DigestLoadThreshold.
const DefaultDigestLoadThreshold = 3

// TaskService implements the task use cases.
type TaskService struct {
	deps    Deps
	cfg     Config
	now     func() time.Time
	wake    func()
	metrics *observability.Metrics
}

// Option configures a TaskService.
type Option func(*TaskService)

// WithClock replaces the time source.
func WithClock(now func() time.Time) Option { return func(s *TaskService) { s.now = now } }

// WithWake sets the function called after a commit that wrote outbox rows,
// so the relay can send them without waiting for its next tick. It must not
// block.
func WithWake(wake func()) Option { return func(s *TaskService) { s.wake = wake } }

// WithMetrics sets where skipped agent jobs are counted. Without it nothing
// is recorded.
func WithMetrics(m *observability.Metrics) Option { return func(s *TaskService) { s.metrics = m } }

// NewTaskService returns a TaskService.
func NewTaskService(d Deps, cfg Config, opts ...Option) *TaskService {
	if cfg.OutboxMaxAttempts <= 0 {
		cfg.OutboxMaxAttempts = 10
	}
	if cfg.DigestLoadThreshold <= 0 {
		cfg.DigestLoadThreshold = DefaultDigestLoadThreshold
	}
	s := &TaskService{deps: d, cfg: cfg, now: time.Now, wake: func() {}}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *TaskService) clock() time.Time {
	// PostgreSQL stores microseconds; truncating here keeps what callers see
	// equal to what a later read returns.
	return s.now().UTC().Truncate(time.Microsecond)
}

// CreateWorkspace creates a workspace in a tenant.
func (s *TaskService) CreateWorkspace(ctx context.Context, tenantID, id, name string) (domain.Workspace, error) {
	w, err := domain.NewWorkspace(tenantID, id, name, s.clock())
	if err != nil {
		return domain.Workspace{}, err
	}
	if err := s.deps.Workspaces.Create(ctx, w); err != nil {
		return domain.Workspace{}, err
	}
	return w, nil
}

// CreateTask creates a task in an existing workspace. The actor becomes its
// creator. A task a person creates also queues triage, if the workspace
// allows it.
func (s *TaskService) CreateTask(ctx context.Context, actor domain.Actor, in domain.NewTaskInput) (*domain.Task, error) {
	now := s.clock()
	task, events, err := domain.NewTask(in, actor, now)
	if err != nil {
		return nil, err
	}
	var skipped []skippedJob
	err = s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		w, err := s.deps.Workspaces.Get(ctx, task.TenantID, task.WorkspaceID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("%w: workspace %q does not exist", domain.ErrValidation, task.WorkspaceID)
			}
			return err
		}
		if err := s.deps.Tasks.Create(ctx, task); err != nil {
			return err
		}
		if err := s.emit(ctx, events, now); err != nil {
			return err
		}
		skipped, err = s.queueAgentJobs(ctx, task, w.AgentSettings, domain.CreateTriggers(actor), actor, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.signal(events)
	s.countSkipped(skipped)
	return task, nil
}

// UpdateTask applies a patch if the task is still at expectedVersion. A patch
// that changes nothing returns the task unchanged.
func (s *TaskService) UpdateTask(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID, expectedVersion int, p domain.Patch) (*domain.Task, error) {
	return s.mutate(ctx, tenantID, id, &expectedVersion, func(t *domain.Task, now time.Time) ([]domain.Event, error) {
		return t.Apply(p, actor, now)
	})
}

// CompleteTask marks a task completed. expectedVersion may be nil, as for a
// "Mark done" click in a notification. Completing a completed task is a
// no-op, so a repeated click is harmless.
func (s *TaskService) CompleteTask(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID, expectedVersion *int) (*domain.Task, error) {
	return s.mutate(ctx, tenantID, id, expectedVersion, func(t *domain.Task, now time.Time) ([]domain.Event, error) {
		return t.TransitionTo(domain.StatusCompleted, actor, now)
	})
}

// ArchiveTask soft-deletes a task. Archiving an archived task is a no-op.
func (s *TaskService) ArchiveTask(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID, expectedVersion *int) (*domain.Task, error) {
	return s.mutate(ctx, tenantID, id, expectedVersion, func(t *domain.Task, now time.Time) ([]domain.Event, error) {
		return t.TransitionTo(domain.StatusArchived, actor, now)
	})
}

// RestoreTask returns an archived task to todo. It fails for a task that is
// not archived, rather than moving it to todo by another route.
func (s *TaskService) RestoreTask(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID, expectedVersion *int) (*domain.Task, error) {
	return s.mutate(ctx, tenantID, id, expectedVersion, func(t *domain.Task, now time.Time) ([]domain.Event, error) {
		if t.Status != domain.StatusArchived {
			return nil, &domain.TransitionError{From: t.Status, To: domain.StatusTodo}
		}
		return t.TransitionTo(domain.StatusTodo, actor, now)
	})
}

// UnblockTask moves a blocked task to in_progress, as when the owner of a
// blocking task clicks "Unblock and set In Progress" on an escalation
// (M9-04). A manual block and a dependency block are both lifted: the person
// decided the task can go ahead. A task already in progress is left alone,
// so a repeated click is harmless; any other status is an invalid
// transition. It is the person's change and notifies as one.
func (s *TaskService) UnblockTask(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID) (*domain.Task, error) {
	return s.mutate(ctx, tenantID, id, nil, func(t *domain.Task, now time.Time) ([]domain.Event, error) {
		switch t.Status {
		case domain.StatusInProgress:
			return nil, nil
		case domain.StatusBlocked:
			return t.TransitionTo(domain.StatusInProgress, actor, now)
		}
		return nil, &domain.TransitionError{From: t.Status, To: domain.StatusInProgress}
	})
}

// SnoozeTask moves the task's reminder to now plus d, as when a user clicks
// "Snooze 2h" in a notification. It raises no event.
func (s *TaskService) SnoozeTask(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID, d time.Duration) (*domain.Task, error) {
	return s.mutate(ctx, tenantID, id, nil, func(t *domain.Task, now time.Time) ([]domain.Event, error) {
		return t.Apply(domain.Patch{RemindAt: domain.SetTime(now.Add(d))}, actor, now)
	})
}

// DeleteTask removes a task permanently, for data-scrubbing requests. In one
// transaction it deletes every outbox row for the task, in any status, so no
// notification about it is sent afterwards, and then the task with its
// watchers and comments. Callers must restrict it to admins.
//
// A relay worker that claimed one of the rows just before may still send it
// once; recording its result then matches no row and is not an error.
//
// The task's dependents lose their link to it through the cascade, and are
// then re-checked as if it had been resolved: a dependent whose only open
// blocker was this task is unblocked. The check runs after the delete, in the
// same transaction, so the deleted task no longer counts as an open blocker.
// Agent jobs for the task go with it through ON DELETE CASCADE.
//
// Morning briefs that name the task are deleted too, in the same
// transaction (AP-08): the digest jobs whose stored report lists it, and
// every outbox row of theirs, so GET /me/brief falls back to an older brief
// or 404 and no pending notification names the task. The tenant's brief
// lock is taken first, exclusive: a brief being stored when the delete
// starts commits first and is then found and deleted, and one stored after
// it finds the task gone and is gathered again (agent.TaskNamer).
func (s *TaskService) DeleteTask(ctx context.Context, tenantID string, id uuid.UUID) error {
	now := s.clock()
	var events []domain.Event
	err := s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		if s.deps.AgentJobs != nil {
			// Before any task lock, so it cannot deadlock with the
			// dependents locked below.
			if err := s.deps.AgentJobs.LockBriefs(ctx, tenantID, true); err != nil {
				return err
			}
		}
		task, err := s.deps.Tasks.GetForUpdate(ctx, tenantID, id)
		if err != nil {
			return err
		}
		dependents, err := s.deps.Dependencies.ListDependents(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if _, err := s.deps.Outbox.DeleteForAggregate(ctx, tenantID, id); err != nil {
			return err
		}
		if s.deps.AgentJobs != nil {
			briefs, err := s.deps.AgentJobs.DeleteBriefsMentioning(ctx, tenantID, id)
			if err != nil {
				return err
			}
			for _, job := range briefs {
				if _, err := s.deps.Outbox.DeleteForAggregate(ctx, tenantID, job); err != nil {
					return err
				}
			}
		}
		if err := s.deps.Tasks.Delete(ctx, tenantID, id); err != nil {
			return err
		}
		gone := *task
		gone.Status = domain.BlockerDeleted
		events, err = s.resolveEach(ctx, &gone, dependents, now)
		return err
	})
	if err != nil {
		return err
	}
	s.signal(events)
	return nil
}

// mutate loads and locks a task, checks its version, applies change, and
// writes the task, its outbox rows, and the agent jobs a person's change
// triggers. Dependency effects run last and, being system changes, queue no
// jobs.
func (s *TaskService) mutate(ctx context.Context, tenantID string, id uuid.UUID, expectedVersion *int,
	change func(t *domain.Task, now time.Time) ([]domain.Event, error)) (*domain.Task, error) {
	now := s.clock()
	var (
		task    *domain.Task
		events  []domain.Event
		skipped []skippedJob
	)
	err := s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		task, err = s.deps.Tasks.GetForUpdate(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if expectedVersion != nil && *expectedVersion != task.Version {
			return &VersionConflictError{Current: task.Version}
		}
		before, from := task.Version, task.Status
		if events, err = change(task, now); err != nil {
			return err
		}
		if task.Version == before {
			return nil // nothing changed
		}
		if err := s.deps.Tasks.Update(ctx, task, before); err != nil {
			return err
		}
		if err := s.emit(ctx, events, now); err != nil {
			return err
		}
		if skipped, err = s.queueForChange(ctx, task, events, now); err != nil {
			return err
		}
		if task.Status == from {
			return nil
		}
		depEvents, err := s.onBlockerStatusChange(ctx, task, from, now)
		events = append(events, depEvents...)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.signal(events)
	s.countSkipped(skipped)
	return task, nil
}

// AddComment adds a comment to a task. It does not change the task's version.
// A person's comment that is the /decompose command also queues a decomposer
// job, if the workspace allows it (section 5.6).
func (s *TaskService) AddComment(ctx context.Context, actor domain.Actor, tenantID string, taskID uuid.UUID, body string) (domain.Comment, error) {
	now := s.clock()
	var (
		comment domain.Comment
		events  []domain.Event
		skipped []skippedJob
	)
	command := actor.Kind == domain.ActorUser && domain.IsDecomposeCommand(body)
	err := s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		get := s.deps.Tasks.Get
		if command {
			get = s.deps.Tasks.GetForUpdate // serialises with RequestDecomposition
		}
		task, err := get(ctx, tenantID, taskID)
		if err != nil {
			return err
		}
		if comment, events, err = domain.NewComment(task, body, actor, now); err != nil {
			return err
		}
		if err := s.deps.Comments.Create(ctx, comment); err != nil {
			return err
		}
		if err := s.emit(ctx, events, now); err != nil {
			return err
		}
		if command {
			skipped, err = s.decomposeFromComment(ctx, task, actor, now)
		}
		return err
	})
	if err != nil {
		return domain.Comment{}, err
	}
	s.signal(events)
	s.countSkipped(skipped)
	return comment, nil
}

// TaskDetail is a task with its most recent comments, newest first.
type TaskDetail struct {
	Task     *domain.Task
	Comments []domain.Comment
}

// GetTask returns a task, including an archived one, with recent comments.
func (s *TaskService) GetTask(ctx context.Context, tenantID string, id uuid.UUID) (TaskDetail, error) {
	task, err := s.deps.Tasks.Get(ctx, tenantID, id)
	if err != nil {
		return TaskDetail{}, err
	}
	comments, err := s.deps.Comments.ListRecent(ctx, id, recentComments)
	if err != nil {
		return TaskDetail{}, err
	}
	if comments == nil {
		comments = []domain.Comment{}
	}
	return TaskDetail{Task: task, Comments: comments}, nil
}

// ListTasks returns a page of tasks. A zero limit means DefaultListLimit;
// larger limits are capped at MaxListLimit.
func (s *TaskService) ListTasks(ctx context.Context, f repository.TaskFilter) (repository.TaskPage, error) {
	switch {
	case f.Limit <= 0:
		f.Limit = DefaultListLimit
	case f.Limit > MaxListLimit:
		f.Limit = MaxListLimit
	}
	if f.Status != "" && !f.Status.Valid() {
		return repository.TaskPage{}, fmt.Errorf("%w: unknown status %q", domain.ErrValidation, f.Status)
	}
	if f.Priority != "" && !f.Priority.Valid() {
		return repository.TaskPage{}, fmt.Errorf("%w: unknown priority %q", domain.ErrValidation, f.Priority)
	}
	if f.IDPrefix != "" {
		if _, _, err := domain.IDPrefixRange(f.IDPrefix); err != nil {
			return repository.TaskPage{}, err
		}
	}
	page, err := s.deps.Tasks.List(ctx, f)
	if page.Tasks == nil {
		page.Tasks = []*domain.Task{}
	}
	return page, err
}

// emit writes the outbox rows for events, after the load gatekeeper has
// decided which assignments wait for the digest.
func (s *TaskService) emit(ctx context.Context, events []domain.Event, now time.Time) error {
	if err := s.deferForLoad(ctx, events); err != nil {
		return err
	}
	entries, err := domain.NewOutboxEntries(events, s.cfg.links(), s.cfg.OutboxMaxAttempts, now)
	if err != nil {
		return err
	}
	// The relay restores this trace when it sends the row, so one trace runs
	// from the request through the outbox to AgentPulse.
	trace := observability.InjectTraceContext(ctx)
	for i := range entries {
		entries[i].TraceContext = trace
	}
	_, err = s.deps.Outbox.Insert(ctx, entries)
	return err
}

// deferForLoad is the digest agent's load gatekeeper (feature A4, M10-01).
// An assignment notification is held for the assignee's digest when they
// already have DigestLoadThreshold or more in_progress p1_critical tasks in
// the tenant, provided the task's workspace allows the digest agent. It is a
// count in the caller's transaction, not a model call, and it queues no job
// (G14). A critical task keeps bypassDigest and is never deferred
// (domain.Event.Deferrable). Every path that raises an assignment, a
// person's or an agent's, passes through here.
//
// The count takes no locks: two assignments racing each other, or a task
// reaching in_progress meanwhile, can only move a borderline notification
// between the digest and immediate delivery.
func (s *TaskService) deferForLoad(ctx context.Context, events []domain.Event) error {
	allowed := map[string]bool{}    // by tenant and workspace
	overloaded := map[string]bool{} // by tenant and assignee
	for i := range events {
		e := &events[i]
		if !e.Deferrable() {
			continue
		}
		t := &e.Task
		key := t.TenantID + "\x00" + t.WorkspaceID
		on, seen := allowed[key]
		if !seen {
			w, err := s.deps.Workspaces.Get(ctx, t.TenantID, t.WorkspaceID)
			if err != nil {
				return err
			}
			on = w.AgentSettings.Allows(domain.AgentDigest)
			allowed[key] = on
		}
		if !on {
			continue
		}
		key = t.TenantID + "\x00" + t.AssigneeID
		over, seen := overloaded[key]
		if !seen {
			n, err := s.deps.Tasks.CountAssigned(ctx, t.TenantID, t.AssigneeID, domain.StatusInProgress, domain.PriorityCritical)
			if err != nil {
				return err
			}
			over = n >= s.cfg.DigestLoadThreshold
			overloaded[key] = over
		}
		e.Deferred = over
	}
	return nil
}

// signal wakes the relay after a commit that may have written outbox rows.
func (s *TaskService) signal(events []domain.Event) {
	if len(events) > 0 {
		s.wake()
	}
}

// VersionConflictError is returned when the caller's version is stale. It
// carries the current version so a client can report it, and matches
// domain.ErrVersionConflict with errors.Is.
type VersionConflictError struct {
	Current int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("version conflict: the task is at version %d", e.Current)
}

func (e *VersionConflictError) Is(target error) bool { return target == domain.ErrVersionConflict }
