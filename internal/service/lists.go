package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// The reads the clients need to list what R1 and R2 expose one at a time
// (M12-04).

// pageLimit applies the list limits shared by every list endpoint.
func pageLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultListLimit
	case n > MaxListLimit:
		return MaxListLimit
	}
	return n
}

// Subtask is a child of a task with the IDs of the tasks it depends on.
type Subtask struct {
	Task      *domain.Task
	DependsOn []uuid.UUID
}

// ListSubtasks returns the children of a task in the tenant, oldest first,
// each with its dependency links. An unknown parent is ErrNotFound.
func (s *TaskService) ListSubtasks(ctx context.Context, tenantID string, parentID uuid.UUID) ([]Subtask, error) {
	if _, err := s.deps.Tasks.Get(ctx, tenantID, parentID); err != nil {
		return nil, err
	}
	children, err := s.deps.Tasks.ListChildren(ctx, tenantID, parentID)
	if err != nil {
		return nil, err
	}
	out := make([]Subtask, 0, len(children))
	for _, c := range children {
		deps, err := s.deps.Dependencies.ListBlockers(ctx, tenantID, c.ID)
		if err != nil {
			return nil, err
		}
		if deps == nil {
			deps = []uuid.UUID{}
		}
		out = append(out, Subtask{Task: c, DependsOn: deps})
	}
	return out, nil
}

// List returns a page of the tenant's agent jobs, newest first.
func (s *AgentJobService) List(ctx context.Context, f repository.AgentJobFilter) (repository.AgentJobPage, error) {
	f.Limit = pageLimit(f.Limit)
	if f.Status != "" && !f.Status.Valid() {
		return repository.AgentJobPage{}, fmt.Errorf("%w: unknown job status %q", domain.ErrValidation, f.Status)
	}
	if f.AgentType != "" && !domain.ValidAgentType(f.AgentType) {
		return repository.AgentJobPage{}, fmt.Errorf("%w: unknown agent type %q", domain.ErrValidation, f.AgentType)
	}
	return s.jobs.List(ctx, f)
}

// JobRuns is an agent job with its model turns.
type JobRuns struct {
	Job  *domain.AgentJob
	Runs []domain.AgentRun
}

// RunsForTask returns the agent activity on a task: its jobs, oldest first,
// each with its run history. An unknown task is ErrNotFound.
func (s *AgentJobService) RunsForTask(ctx context.Context, tenantID string, taskID uuid.UUID) ([]JobRuns, error) {
	if _, err := s.tasks.Get(ctx, tenantID, taskID); err != nil {
		return nil, err
	}
	jobs, err := s.jobs.ListForTask(ctx, tenantID, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]JobRuns, 0, len(jobs))
	for _, j := range jobs {
		runs, err := s.jobs.ListRuns(ctx, j.ID)
		if err != nil {
			return nil, err
		}
		if runs == nil {
			runs = []domain.AgentRun{}
		}
		out = append(out, JobRuns{Job: j, Runs: runs})
	}
	return out, nil
}

// OutboxService serves the admin's view of the outbox: listing rows and
// returning dead-letter rows to delivery.
type OutboxService struct {
	outbox repository.OutboxRepository
	now    func() time.Time
	wake   func()
}

// NewOutboxService returns an OutboxService. wake, which may be nil, tells
// the relay a row is due, as after a task change.
func NewOutboxService(outbox repository.OutboxRepository, now func() time.Time, wake func()) *OutboxService {
	if now == nil {
		now = time.Now
	}
	return &OutboxService{outbox: outbox, now: now, wake: wake}
}

// List returns a page of the tenant's outbox rows, newest first.
func (s *OutboxService) List(ctx context.Context, f repository.OutboxFilter) (repository.OutboxPage, error) {
	f.Limit = pageLimit(f.Limit)
	switch f.Status {
	case "", domain.OutboxPending, domain.OutboxProcessing, domain.OutboxDispatched, domain.OutboxDeadLetter:
	default:
		return repository.OutboxPage{}, fmt.Errorf("%w: unknown outbox status %q", domain.ErrValidation, f.Status)
	}
	return s.outbox.List(ctx, f)
}

// Replay returns the tenant's dead-letter row to pending, so the relay sends
// it again with a fresh count of attempts (section 4.3, "manual replay").
func (s *OutboxService) Replay(ctx context.Context, tenantID string, id uuid.UUID) (domain.OutboxEntry, error) {
	e, err := s.outbox.Replay(ctx, tenantID, id, s.now().UTC())
	if err != nil {
		return domain.OutboxEntry{}, err
	}
	if s.wake != nil {
		s.wake()
	}
	return e, nil
}
