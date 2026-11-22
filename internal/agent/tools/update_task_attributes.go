package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// AgentTaskWriter is the part of service.TaskService that agent tools use.
type AgentTaskWriter interface {
	UpdateTaskAsAgent(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID,
		expectedVersion int, p domain.Patch, run domain.AgentRunRecord) (*domain.Task, error)
	RecommendPriority(ctx context.Context, tenantID string, id uuid.UUID, rec domain.PriorityRecommendation) error
}

// UpdateTaskAttributesName is the tool's name (section 5.7).
const UpdateTaskAttributesName = "UpdateTaskAttributes"

// UpdateTaskAttributes sets a task's labels, description, estimate, and
// priority. It is Tier 1, except Tier 2 when priority is set: a priority is
// only ever a recommendation until a person approves it (section 5.7).
// Every applied change is recorded in the task's agent_metadata with the
// model's reasoning.
type UpdateTaskAttributes struct {
	Tasks AgentTaskWriter
}

// UpdateTaskAttributesArgs are the tool's arguments.
type UpdateTaskAttributesArgs struct {
	TaskID           uuid.UUID        `json:"task_id"`
	Labels           *[]string        `json:"labels,omitempty"`
	Description      *string          `json:"description,omitempty"`
	EstimatedMinutes *int             `json:"estimated_minutes,omitempty"`
	Priority         *domain.Priority `json:"priority,omitempty"`
	Reasoning        string           `json:"reasoning"`
}

// UpdateResult is the tool's output. It reports the new version only for the
// job's own task, as VersionedOutput requires.
type UpdateResult struct {
	Version int      `json:"version"`
	Changed []string `json:"changed"`
}

type versionedUpdate struct{ UpdateResult }

func (v versionedUpdate) TaskVersion() int { return v.Version }

func (UpdateTaskAttributes) Name() string { return UpdateTaskAttributesName }

func (UpdateTaskAttributes) Description() string {
	return "Set a task's labels, description, estimate in minutes, or priority, with your reasoning. " +
		"Prefer labels the workspace already uses. A priority is sent to a person for approval."
}

func parseUpdate(args json.RawMessage) (UpdateTaskAttributesArgs, error) {
	var a UpdateTaskAttributesArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("%w: %s", domain.ErrValidation, err.Error())
	}
	if a.Labels == nil && a.Description == nil && a.EstimatedMinutes == nil && a.Priority == nil {
		return a, fmt.Errorf("%w: give at least one of labels, description, estimated_minutes, or priority", domain.ErrValidation)
	}
	return a, nil
}

// Tier is 2 when the call sets a priority.
func (UpdateTaskAttributes) Tier(args json.RawMessage) domain.SafetyTier {
	var a struct {
		Priority *string `json:"priority"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Priority != nil {
		return domain.Tier2
	}
	return domain.Tier1
}

// Proposed stores a proposed priority as the task's recommendation, without
// moving its version (M7-04).
func (u UpdateTaskAttributes) Proposed(ctx context.Context, s Scope, args json.RawMessage) error {
	a, err := parseUpdate(args)
	if err != nil || a.Priority == nil {
		return err
	}
	return u.Tasks.RecommendPriority(ctx, s.TenantID, a.TaskID,
		domain.PriorityRecommendation{Value: *a.Priority, JobID: s.JobID, Reasoning: a.Reasoning})
}

// Execute applies the change with the scope's expected version, as the agent.
func (u UpdateTaskAttributes) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	a, err := parseUpdate(args)
	if err != nil {
		return nil, err
	}
	p := domain.Patch{Labels: a.Labels, Description: a.Description, EstimatedMinutes: a.EstimatedMinutes, Priority: a.Priority}
	changes := map[string]any{}
	if a.Labels != nil {
		changes["labels"] = *a.Labels
	}
	if a.Description != nil {
		changes["description"] = *a.Description
	}
	if a.EstimatedMinutes != nil {
		changes["estimated_minutes"] = *a.EstimatedMinutes
	}
	if a.Priority != nil {
		changes["priority"] = string(*a.Priority)
	}
	task, err := u.Tasks.UpdateTaskAsAgent(ctx, s.Actor(), s.TenantID, a.TaskID, s.BaseVersion, p,
		domain.AgentRunRecord{JobID: s.JobID, Agent: s.AgentType, Changes: changes, Reasoning: a.Reasoning})
	if err != nil {
		return nil, err
	}
	res := UpdateResult{Version: task.Version, Changed: sortedKeys(changes)}
	if s.TaskID != nil && *s.TaskID == task.ID {
		return versionedUpdate{res}, nil
	}
	return res, nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for _, k := range []string{"description", "estimated_minutes", "labels", "priority"} {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	return out
}
