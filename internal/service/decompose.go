package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
)

// ErrAgentDisabled is returned when a person explicitly asks for an agent
// that the workspace has switched off (default G21).
var ErrAgentDisabled = errors.New("agent is disabled for this workspace")

// Decomposition triggers, recorded in the job's input context.
const (
	TriggerDecomposeRequest = "decompose_request"
	TriggerDecomposeComment = "decompose_comment"
)

// RequestDecomposition queues a decomposer job for the task, as POST
// /tasks/{id}/decompose does. It returns the job and whether it is new: while
// an earlier decomposer job for the task is still pending, running, or
// awaiting approval, that job is returned instead, so a repeated click
// cannot split the task twice. The task row is locked, so two concurrent
// requests cannot both queue one.
//
// It fails with ErrAgentDisabled when the workspace has agents off or the
// decomposer disabled, and with ErrValidation for a completed or archived
// task.
func (s *TaskService) RequestDecomposition(ctx context.Context, actor domain.Actor, tenantID string, taskID uuid.UUID) (*domain.AgentJob, bool, error) {
	now := s.clock()
	var (
		job     *domain.AgentJob
		created bool
	)
	err := s.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		task, err := s.deps.Tasks.GetForUpdate(ctx, tenantID, taskID)
		if err != nil {
			return err
		}
		w, err := s.deps.Workspaces.Get(ctx, task.TenantID, task.WorkspaceID)
		if err != nil {
			return err
		}
		if !w.AgentSettings.Allows(domain.AgentDecomposer) {
			reason := "the decomposer is disabled for this workspace"
			if !w.AgentSettings.AgentsEnabled {
				reason = "agents are switched off for this workspace"
			}
			return fmt.Errorf("%w: %s", ErrAgentDisabled, reason)
		}
		job, created, err = s.queueDecomposition(ctx, task, actor, TriggerDecomposeRequest, now)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return job, created, nil
}

// queueDecomposition writes a decomposer job for task, which the caller has
// locked, unless one is still open. The caller has checked the settings.
func (s *TaskService) queueDecomposition(ctx context.Context, task *domain.Task, actor domain.Actor, trigger string, now time.Time) (*domain.AgentJob, bool, error) {
	if !task.IsOpen() {
		return nil, false, fmt.Errorf("%w: a %s task cannot be decomposed", domain.ErrValidation, task.Status)
	}
	jobs, err := s.deps.AgentJobs.ListForTask(ctx, task.TenantID, task.ID)
	if err != nil {
		return nil, false, err
	}
	for _, j := range jobs {
		if j.AgentType == domain.AgentDecomposer && j.Status.Active() {
			return j, false, nil
		}
	}
	input, err := json.Marshal(jobInput{Trigger: trigger, ActorID: actor.ID, TraceContext: observability.InjectTraceContext(ctx)})
	if err != nil {
		return nil, false, err
	}
	request, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	taskID, version := task.ID, task.Version
	job, err := domain.NewAgentJob(domain.NewAgentJobInput{
		TenantID:        task.TenantID,
		TaskID:          &taskID,
		AgentType:       domain.AgentDecomposer,
		DedupKey:        domain.RequestJobDedupKey(domain.AgentDecomposer, taskID, request),
		BaseTaskVersion: &version,
		InputContext:    input,
	}, now)
	if err != nil {
		return nil, false, err
	}
	if _, err := s.deps.AgentJobs.Insert(ctx, job); err != nil {
		return nil, false, err
	}
	return &job, true, nil
}

// decomposeFromComment queues a decomposer job for a /decompose comment, in
// the comment's transaction. Unlike the explicit request, the comment is
// kept when the decomposer is off or the task is closed: the job is skipped
// and counted, as for any other trigger.
func (s *TaskService) decomposeFromComment(ctx context.Context, task *domain.Task, actor domain.Actor, now time.Time) ([]skippedJob, error) {
	w, err := s.deps.Workspaces.Get(ctx, task.TenantID, task.WorkspaceID)
	if err != nil {
		return nil, err
	}
	switch {
	case !w.AgentSettings.AgentsEnabled:
		return []skippedJob{{domain.AgentDecomposer, observability.SkipAgentsOff}}, nil
	case !w.AgentSettings.Allows(domain.AgentDecomposer):
		return []skippedJob{{domain.AgentDecomposer, observability.SkipAgentOff}}, nil
	case !task.IsOpen():
		return nil, nil
	}
	_, created, err := s.queueDecomposition(ctx, task, actor, TriggerDecomposeComment, now)
	if err != nil || created {
		return nil, err
	}
	return []skippedJob{{domain.AgentDecomposer, observability.SkipDuplicate}}, nil
}
