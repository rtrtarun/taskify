package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// ReassignTaskName is the tool's name (section 5.7).
const ReassignTaskName = "ReassignTask"

// ReassignTask gives a task to another person. It is always Tier 2: who
// owns the work is a person's decision, so a call is stored as a proposal and
// runs only when approved, as the agent and against the version the job
// started from.
type ReassignTask struct {
	Tasks AgentTaskWriter
}

// ReassignTaskArgs are the tool's arguments.
type ReassignTaskArgs struct {
	TaskID     uuid.UUID `json:"task_id"`
	AssigneeID string    `json:"assignee_id"`
	Reasoning  string    `json:"reasoning"`
}

func (ReassignTask) Name() string { return ReassignTaskName }

func (ReassignTask) Description() string {
	return "Propose giving a task to another person, with your reasoning. A person approves it first."
}

func (ReassignTask) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "format": "uuid", "description": "The task to reassign."},
			"assignee_id": {"type": "string", "minLength": 1, "maxLength": 128,
				"description": "The user ID of the new assignee."},
			"reasoning": {"type": "string", "minLength": 1, "maxLength": 1000,
				"description": "Why this person, in one or two sentences."}
		},
		"required": ["task_id", "assignee_id", "reasoning"],
		"additionalProperties": false
	}`)
}

func (ReassignTask) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier2 }

// Execute applies an approved reassignment.
func (r ReassignTask) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	var a ReassignTaskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err.Error())
	}
	return applyAgentPatch(ctx, r.Tasks, s, a.TaskID, domain.Patch{AssigneeID: &a.AssigneeID},
		map[string]any{"assignee_id": a.AssigneeID}, a.Reasoning)
}

// applyAgentPatch applies a single-field change as the agent at the scope's
// expected version and records it with the reasoning. The output reports
// the new version when the task is the job's own.
func applyAgentPatch(ctx context.Context, w AgentTaskWriter, s Scope, taskID uuid.UUID, p domain.Patch,
	changes map[string]any, reasoning string) (any, error) {
	task, err := w.UpdateTaskAsAgent(ctx, s.Actor(), s.TenantID, taskID, s.BaseVersion, p,
		domain.AgentRunRecord{JobID: s.JobID, Agent: s.AgentType, Changes: changes, Reasoning: reasoning})
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	res := UpdateResult{Version: task.Version, Changed: keys}
	if s.TaskID != nil && *s.TaskID == task.ID {
		return versionedUpdate{res}, nil
	}
	return res, nil
}
