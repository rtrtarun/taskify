package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// ShiftDueDateName is the tool's name (section 5.7).
const ShiftDueDateName = "ShiftDueDate"

// ShiftDueDate moves a task's due date. It is always Tier 2: section 5.7
// takes the stricter reading of the source documents, so every shift, of any
// size and in either direction, waits for a person's approval.
type ShiftDueDate struct {
	Tasks AgentTaskWriter
}

// ShiftDueDateArgs are the tool's arguments.
type ShiftDueDateArgs struct {
	TaskID    uuid.UUID `json:"task_id"`
	DueAt     time.Time `json:"due_at"`
	Reasoning string    `json:"reasoning"`
}

func (ShiftDueDate) Name() string { return ShiftDueDateName }

func (ShiftDueDate) Description() string {
	return "Propose a new due date for a task, with your reasoning. A person approves it first."
}

func (ShiftDueDate) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier2 }

// Execute applies an approved due date.
func (d ShiftDueDate) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	var a ShiftDueDateArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err.Error())
	}
	due := a.DueAt.UTC()
	return applyAgentPatch(ctx, d.Tasks, s, a.TaskID, domain.Patch{DueAt: domain.SetTime(due)},
		map[string]any{"due_at": due.Format(time.RFC3339)}, a.Reasoning)
}
