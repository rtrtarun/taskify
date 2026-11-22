package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// UpdateTaskAsAgent applies an agent's patch if the task is still at
// expectedVersion, and records the change and its reasoning in the task's
// agent_metadata in the same write (section 5.7). A patch that changes
// nothing records nothing. actor must be an agent: its change notifies
// people but queues no agent work.
func (s *TaskService) UpdateTaskAsAgent(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID,
	expectedVersion int, p domain.Patch, run domain.AgentRunRecord) (*domain.Task, error) {
	if actor.Kind != domain.ActorAgent {
		return nil, fmt.Errorf("%w: UpdateTaskAsAgent needs an agent actor", domain.ErrValidation)
	}
	return s.mutate(ctx, tenantID, id, &expectedVersion, func(t *domain.Task, now time.Time) ([]domain.Event, error) {
		before := t.Version
		events, err := t.Apply(p, actor, now)
		if err != nil || t.Version == before {
			return events, err
		}
		run.At = now
		t.RecordAgentRun(run)
		return events, nil
	})
}

// AddAgentComment adds an agent's comment to a task and records the run in
// the task's agent_metadata, in one transaction (section 5.7). The comment is
// authored by the agent and sends task.comment_added like any other; a
// comment is not an edit, so the task's version is unchanged and no agent job
// is queued. actor must be an agent.
//
// A job comments on a task at most once: if run.JobID already recorded a
// comment there, as after a retry, that comment's ID is returned with
// created false and nothing is written.
func (s *TaskService) AddAgentComment(ctx context.Context, actor domain.Actor, tenantID string, taskID uuid.UUID,
	body string, run domain.AgentRunRecord) (commentID uuid.UUID, created bool, err error) {
	if actor.Kind != domain.ActorAgent {
		return uuid.Nil, false, fmt.Errorf("%w: AddAgentComment needs an agent actor", domain.ErrValidation)
	}
	now := s.clock()
	var events []domain.Event
	err = s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		task, err := s.deps.Tasks.GetForUpdate(ctx, tenantID, taskID)
		if err != nil {
			return err
		}
		if id, ok := task.CommentOf(run.JobID); ok {
			commentID = id
			return nil
		}
		comment, evs, err := domain.NewComment(task, body, actor, now)
		if err != nil {
			return err
		}
		if err := s.deps.Comments.Create(ctx, comment); err != nil {
			return err
		}
		if run.Changes == nil {
			run.Changes = map[string]any{}
		}
		run.Changes[domain.RunCommentID] = comment.ID.String()
		run.At = now
		task.RecordAgentRun(run)
		if err := s.deps.Tasks.Update(ctx, task, task.Version); err != nil {
			return err
		}
		if err := s.emit(ctx, evs, now); err != nil {
			return err
		}
		events, commentID, created = evs, comment.ID, true
		return nil
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	s.signal(events)
	return commentID, created, nil
}

// RecommendPriority stores an agent's suggested priority on the task. It
// changes no field a person edits, so the version, updated_at, and outbox
// are untouched: a proposal based on the current version stays current.
func (s *TaskService) RecommendPriority(ctx context.Context, tenantID string, id uuid.UUID, rec domain.PriorityRecommendation) error {
	rec.At = s.clock()
	return s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		task, err := s.deps.Tasks.GetForUpdate(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if err := task.RecommendPriority(rec); err != nil {
			return err
		}
		return s.deps.Tasks.Update(ctx, task, task.Version)
	})
}

// AddDependencies links a task to several blockers in one transaction: all
// links are made, or none. The rules are those of AddDependency.
func (s *TaskService) AddDependencies(ctx context.Context, tenantID string, taskID uuid.UUID, dependsOn []uuid.UUID) (*domain.Task, error) {
	var task *domain.Task
	err := s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		for _, b := range dependsOn {
			t, err := s.AddDependency(ctx, tenantID, taskID, b)
			if err != nil {
				return err
			}
			task = t
		}
		if task == nil {
			var err error
			task, err = s.deps.Tasks.Get(ctx, tenantID, taskID)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}
