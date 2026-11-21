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
