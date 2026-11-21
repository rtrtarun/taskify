package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
)

// skippedJob is a job a change asked for but did not queue. It is counted
// only after the change commits, so a rolled-back change counts nothing.
type skippedJob struct{ agent, reason string }

// jobInput is a queued job's input_context. Tenant and task scope are columns
// of the job row, never part of this.
type jobInput struct {
	Trigger string `json:"trigger"`
	ActorID string `json:"actor_id"`
	// TraceContext carries the request's trace, so the agent's run joins it.
	TraceContext map[string]string `json:"trace_context,omitempty"`
}

// queueAgentJobs writes a pending job for each trigger the workspace's
// settings allow, in the caller's transaction: the job exists exactly when
// the change commits. A job already queued for the same agent and task
// version is not written again.
func (s *TaskService) queueAgentJobs(ctx context.Context, task *domain.Task, settings domain.AgentSettings,
	triggers []domain.AgentTrigger, actor domain.Actor, now time.Time) ([]skippedJob, error) {
	var skipped []skippedJob
	for _, tr := range triggers {
		switch {
		case !settings.AgentsEnabled:
			skipped = append(skipped, skippedJob{tr.AgentType, observability.SkipAgentsOff})
			continue
		case !settings.Allows(tr.AgentType):
			skipped = append(skipped, skippedJob{tr.AgentType, observability.SkipAgentOff})
			continue
		}
		input, err := json.Marshal(jobInput{Trigger: tr.Event, ActorID: actor.ID,
			TraceContext: observability.InjectTraceContext(ctx)})
		if err != nil {
			return nil, err
		}
		taskID, version := task.ID, task.Version
		job, err := domain.NewAgentJob(domain.NewAgentJobInput{
			TenantID:        task.TenantID,
			TaskID:          &taskID,
			AgentType:       tr.AgentType,
			DedupKey:        domain.EventJobDedupKey(tr.AgentType, taskID, version),
			BaseTaskVersion: &version,
			InputContext:    input,
		}, now)
		if err != nil {
			return nil, err
		}
		created, err := s.deps.AgentJobs.Insert(ctx, job)
		if err != nil {
			return nil, err
		}
		if !created {
			skipped = append(skipped, skippedJob{tr.AgentType, observability.SkipDuplicate})
		}
	}
	return skipped, nil
}

// queueForChange queues the jobs a change to an existing task triggers. It
// reads the workspace's settings only when there is something to queue, so
// agent and system changes cost no extra query.
func (s *TaskService) queueForChange(ctx context.Context, task *domain.Task, events []domain.Event, now time.Time) ([]skippedJob, error) {
	triggers := domain.ChangeTriggers(events)
	if len(triggers) == 0 {
		return nil, nil
	}
	w, err := s.deps.Workspaces.Get(ctx, task.TenantID, task.WorkspaceID)
	if err != nil {
		return nil, err
	}
	// ChangeTriggers keeps only a person's events, all from the same actor.
	return s.queueAgentJobs(ctx, task, w.AgentSettings, triggers, actorOf(events), now)
}

// actorOf returns the person behind the first user event.
func actorOf(events []domain.Event) domain.Actor {
	for _, e := range events {
		if e.Actor.Kind == domain.ActorUser {
			return e.Actor
		}
	}
	return domain.Actor{}
}

// countSkipped records skipped jobs after a commit.
func (s *TaskService) countSkipped(skipped []skippedJob) {
	for _, j := range skipped {
		s.metrics.AgentJobSkipped(j.agent, j.reason)
	}
}
