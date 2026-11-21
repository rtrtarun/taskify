// Package agent runs agent jobs: it claims queued jobs, holds a bounded
// conversation with the model, and turns the model's tool calls into checked
// actions through the tool registry (docs/architecture.md sections 3.6 and
// 5.3).
//
// The model never touches the database. Tenant and task scope come from the
// agent_jobs row, and every change goes through a registered tool.
package agent

import (
	"context"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/domain"
)

// Agent is one kind of agent, such as triage. The runtime owns the loop;
// an agent only says which tools it may use and what the model is told.
//
// Which task changes queue an agent's jobs is decided in the domain
// (domain.CreateTriggers and domain.ChangeTriggers), inside the task
// transaction, so the interface has no Triggers method.
type Agent interface {
	// Type is the agent type, one of domain.AgentTypes.
	Type() string
	// AllowedTools are the registered tools offered to the model.
	AllowedTools() []string
	// BuildContext loads what the model needs and returns the opening
	// messages. It runs at the start of every run, so a job re-run after a
	// version conflict sees the person's edit.
	BuildContext(ctx context.Context, job *domain.AgentJob) ([]llm.Message, error)
}
