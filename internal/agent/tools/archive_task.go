package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// ArchiveTaskName is the tool's name (section 5.7).
const ArchiveTaskName = "ArchiveTask"

// ArchiveTask archives a task. It is always Tier 2, and no agent is offered
// it by default (section 5.7). Archiving releases the task's dependents as
// any archive does, so an approved call can unblock other work.
type ArchiveTask struct {
	Tasks AgentTaskWriter
}

// ArchiveTaskArgs are the tool's arguments.
type ArchiveTaskArgs struct {
	TaskID    uuid.UUID `json:"task_id"`
	Reasoning string    `json:"reasoning"`
}

func (ArchiveTask) Name() string { return ArchiveTaskName }

func (ArchiveTask) Description() string {
	return "Propose archiving a task that is no longer needed, with your reasoning. A person approves it first."
}

func (ArchiveTask) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier2 }

// Execute applies an approved archive.
func (t ArchiveTask) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	var a ArchiveTaskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err.Error())
	}
	archived := domain.StatusArchived
	return applyAgentPatch(ctx, t.Tasks, s, a.TaskID, domain.Patch{Status: &archived},
		map[string]any{"status": string(archived)}, a.Reasoning)
}
