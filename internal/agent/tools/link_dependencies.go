package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// DependencyLinker is the part of service.TaskService LinkDependencies uses.
type DependencyLinker interface {
	AddDependencies(ctx context.Context, tenantID string, taskID uuid.UUID, dependsOn []uuid.UUID) (*domain.Task, error)
}

// LinkDependenciesName is the tool's name (section 5.7).
const LinkDependenciesName = "LinkDependencies"

// LinkDependencies records that a task cannot start until others are done.
// It is Tier 1 and goes through TaskService.AddDependencies, so the cycle,
// tenant, and blocking rules of M5-03 apply unchanged: a link that would
// close a cycle comes back to the model as a rejected call.
type LinkDependencies struct {
	Tasks DependencyLinker
}

// LinkDependenciesArgs are the tool's arguments.
type LinkDependenciesArgs struct {
	TaskID           uuid.UUID   `json:"task_id"`
	DependsOnTaskIDs []uuid.UUID `json:"depends_on_task_ids"`
	Reasoning        string      `json:"reasoning,omitempty"`
}

// LinkResult is the tool's output.
type LinkResult struct {
	Linked  int    `json:"linked"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

type versionedLink struct{ LinkResult }

func (v versionedLink) TaskVersion() int { return v.Version }

func (LinkDependencies) Name() string { return LinkDependenciesName }

func (LinkDependencies) Description() string {
	return "Record that a task cannot start until other existing tasks are completed. " +
		"Use only when the dependency is clear from the tasks themselves."
}

func (LinkDependencies) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "format": "uuid", "description": "The task that has to wait."},
			"depends_on_task_ids": {"type": "array", "items": {"type": "string", "format": "uuid"},
				"minItems": 1, "maxItems": 10, "uniqueItems": true, "description": "The tasks it waits for."},
			"reasoning": {"type": "string", "maxLength": 1000}
		},
		"required": ["task_id", "depends_on_task_ids"],
		"additionalProperties": false
	}`)
}

func (LinkDependencies) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier1 }

// Execute links the task to every blocker in one transaction.
func (l LinkDependencies) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	var a LinkDependenciesArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	task, err := l.Tasks.AddDependencies(ctx, s.TenantID, a.TaskID, a.DependsOnTaskIDs)
	if err != nil {
		return nil, err
	}
	// Linking may block a todo task, which moves its version.
	res := LinkResult{Linked: len(a.DependsOnTaskIDs), Status: string(task.Status), Version: task.Version}
	if s.TaskID != nil && *s.TaskID == task.ID {
		return versionedLink{res}, nil
	}
	return res, nil
}
