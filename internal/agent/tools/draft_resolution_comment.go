package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// AgentCommenter is the part of service.TaskService DraftResolutionComment
// uses.
type AgentCommenter interface {
	AddAgentComment(ctx context.Context, actor domain.Actor, tenantID string, taskID uuid.UUID,
		body string, run domain.AgentRunRecord) (uuid.UUID, bool, error)
}

// DraftResolutionCommentName is the tool's name (section 5.7).
const DraftResolutionCommentName = "DraftResolutionComment"

// DraftResolutionComment posts a resolution brief on a task as a comment
// authored by the agent. It is Tier 1: a comment changes no field of the
// task and is visibly the agent's. The run is recorded in the task's
// agent_metadata with the model's reasoning, and a job posts at most one
// comment per task, so a retried run does not post the brief twice.
type DraftResolutionComment struct {
	Tasks AgentCommenter
}

// DraftResolutionCommentArgs are the tool's arguments.
type DraftResolutionCommentArgs struct {
	TaskID    uuid.UUID `json:"task_id"`
	Body      string    `json:"body"`
	Reasoning string    `json:"reasoning"`
}

// CommentResult is the tool's output. Created is false when the job had
// already commented on the task.
type CommentResult struct {
	CommentID uuid.UUID `json:"comment_id"`
	Created   bool      `json:"created"`
}

func (DraftResolutionComment) Name() string { return DraftResolutionCommentName }

func (DraftResolutionComment) Description() string {
	return "Post a resolution brief on a blocked or stalled task as your comment: what is in the way, " +
		"who can clear it, and the next concrete step. Call it once per task."
}

func (DraftResolutionComment) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "format": "uuid", "description": "The task to comment on."},
			"body": {"type": "string", "minLength": 1, "maxLength": 4000,
				"description": "The comment, in plain text or Markdown, written for the people on the task."},
			"reasoning": {"type": "string", "minLength": 1, "maxLength": 1000,
				"description": "Why this brief, in one or two sentences."}
		},
		"required": ["task_id", "body", "reasoning"],
		"additionalProperties": false
	}`)
}

func (DraftResolutionComment) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier1 }

// Execute adds the comment as the agent.
func (d DraftResolutionComment) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	var a DraftResolutionCommentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err.Error())
	}
	id, created, err := d.Tasks.AddAgentComment(ctx, s.Actor(), s.TenantID, a.TaskID, a.Body,
		domain.AgentRunRecord{JobID: s.JobID, Agent: s.AgentType, Reasoning: a.Reasoning})
	if err != nil {
		return nil, err
	}
	return CommentResult{CommentID: id, Created: created}, nil
}
