// Package agenttest holds a reference agent and tool that exercise the agent
// engine end to end in tests (task M5-10). They are deliberately minimal:
// real agents start in M7.
package agenttest

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
)

// TaskUpdater is the part of service.TaskService the tool uses.
type TaskUpdater interface {
	UpdateTask(ctx context.Context, actor domain.Actor, tenantID string, id uuid.UUID, expectedVersion int, p domain.Patch) (*domain.Task, error)
}

// TaskGetter loads a task within a tenant.
type TaskGetter interface {
	Get(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Task, error)
}

// DescribeToolName is the reference tool's name.
const DescribeToolName = "Describe"

// DescribeTool sets a task's description through TaskService, with the
// scope's expected version and the agent as actor. Tier sets its safety
// tier; zero means Tier 1.
type DescribeTool struct {
	Tasks TaskUpdater
	Tier2 bool
}

// DescribeArgs are the tool's arguments.
type DescribeArgs struct {
	TaskID      uuid.UUID `json:"task_id"`
	Description string    `json:"description"`
}

// DescribeOutput reports the task's new version, so later calls in the same
// run expect it.
type DescribeOutput struct {
	Version int `json:"version"`
}

// TaskVersion implements tools.VersionedOutput.
func (o DescribeOutput) TaskVersion() int { return o.Version }

func (DescribeTool) Name() string        { return DescribeToolName }
func (DescribeTool) Description() string { return "Set the task description." }
func (DescribeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {
		"task_id": {"type": "string", "format": "uuid"},
		"description": {"type": "string", "minLength": 1}},
		"required": ["task_id", "description"], "additionalProperties": false}`)
}

func (d DescribeTool) Tier(json.RawMessage) domain.SafetyTier {
	if d.Tier2 {
		return domain.Tier2
	}
	return domain.Tier1
}

func (d DescribeTool) Execute(ctx context.Context, s tools.Scope, args json.RawMessage) (any, error) {
	var a DescribeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	task, err := d.Tasks.UpdateTask(ctx, s.Actor(), s.TenantID, a.TaskID, s.BaseVersion, domain.Patch{Description: &a.Description})
	if err != nil {
		return nil, err
	}
	return DescribeOutput{Version: task.Version}, nil
}

// Agent is the reference agent: it shows the model the task and offers the
// Describe tool.
type Agent struct {
	AgentType string // default triage
	Tasks     TaskGetter
}

func (a Agent) Type() string {
	if a.AgentType == "" {
		return domain.AgentTriage
	}
	return a.AgentType
}

func (Agent) AllowedTools() []string { return []string{DescribeToolName} }

func (a Agent) BuildContext(ctx context.Context, job *domain.AgentJob) ([]llm.Message, error) {
	task, err := a.Tasks.Get(ctx, job.TenantID, *job.TaskID)
	if err != nil {
		return nil, err
	}
	return []llm.Message{
		{Role: llm.RoleSystem, Content: "You describe tasks. Call Describe once, then answer briefly."},
		{Role: llm.RoleUser, Content: "Task " + task.ID.String() + ": " + task.Title},
	}, nil
}
