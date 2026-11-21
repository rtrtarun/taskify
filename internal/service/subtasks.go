package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// CreateSubtasks splits a parent task into children with dependencies
// between them, in one transaction (section 3.8): either every child and
// every link is created, or nothing is. actor must be an agent; creatorID is
// the person who asked for the decomposition.
//
//   - The parent must still be at expectedVersion, so a person's edit made
//     while the agent worked wins with a version conflict, and still open.
//   - The plan is checked with domain.PlanSubtasks against the workspace's
//     current max_subtasks_per_decomposition.
//   - Children with blockers start blocked with reason dependency and send
//     nothing; the others send task.created. No agent job is queued.
//   - The parent's agent_metadata records the run, without moving its
//     version: the parent's own fields are unchanged.
//
// A job creates its subtasks at most once. If run.JobID already recorded a
// decomposition on the parent, as after a retry, the earlier subtasks are
// returned with Created false.
//
// The links join only tasks created in this transaction, which no other
// transaction can see or link to yet, so no tenant lock is needed for the
// cycle check (compare AddDependency).
func (s *TaskService) CreateSubtasks(ctx context.Context, actor domain.Actor, tenantID string, parentID uuid.UUID,
	expectedVersion int, creatorID string, specs []domain.SubtaskSpec, run domain.AgentRunRecord) (domain.SubtaskResult, error) {
	if actor.Kind != domain.ActorAgent {
		return domain.SubtaskResult{}, fmt.Errorf("%w: CreateSubtasks needs an agent actor", domain.ErrValidation)
	}
	now := s.clock()
	var (
		res    domain.SubtaskResult
		events []domain.Event
	)
	err := s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		parent, err := s.deps.Tasks.GetForUpdate(ctx, tenantID, parentID)
		if err != nil {
			return err
		}
		if parent.Version != expectedVersion {
			return &VersionConflictError{Current: parent.Version}
		}
		if rec, done := parent.RunOf(run.JobID); done {
			for _, id := range rec.SubtaskIDs() {
				child, err := s.deps.Tasks.Get(ctx, tenantID, id)
				if errors.Is(err, domain.ErrNotFound) {
					continue // deleted since
				}
				if err != nil {
					return err
				}
				res.Subtasks = append(res.Subtasks, child)
			}
			return nil
		}
		if !parent.IsOpen() {
			return fmt.Errorf("%w: a %s task cannot be decomposed", domain.ErrValidation, parent.Status)
		}
		w, err := s.deps.Workspaces.Get(ctx, parent.TenantID, parent.WorkspaceID)
		if err != nil {
			return err
		}
		plan, err := domain.PlanSubtasks(specs, w.AgentSettings.MaxSubtasksPerDecomposition)
		if err != nil {
			return err
		}

		titles, ids := make([]string, len(plan)), make([]string, len(plan))
		for i, p := range plan {
			child, evs, err := domain.NewSubtask(parent, p, creatorID, actor, now)
			if err != nil {
				return err
			}
			if err := s.deps.Tasks.Create(ctx, child); err != nil {
				return err
			}
			for _, b := range p.Blockers {
				dep, err := domain.NewDependency(child, res.Subtasks[b], now)
				if err != nil {
					return err
				}
				if _, err := s.deps.Dependencies.Create(ctx, dep); err != nil {
					return err
				}
			}
			res.Subtasks = append(res.Subtasks, child)
			events = append(events, evs...)
			titles[i], ids[i] = child.Title, child.ID.String()
		}

		if run.Changes == nil {
			run.Changes = map[string]any{}
		}
		run.Changes["subtasks"] = titles
		run.Changes["subtask_ids"] = ids
		run.At = now
		parent.RecordAgentRun(run)
		if err := s.deps.Tasks.Update(ctx, parent, parent.Version); err != nil {
			return err
		}
		res.Created = true
		return s.emit(ctx, events, now)
	})
	if err != nil {
		return domain.SubtaskResult{}, err
	}
	s.signal(events)
	return res, nil
}
