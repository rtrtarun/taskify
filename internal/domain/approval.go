package domain

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

// EventAgentApprovalRequested is raised when an agent job stores proposals
// for a person to approve (section 3.7).
const EventAgentApprovalRequested = "agent.approval_requested"

// JobRef identifies the agent job an approval request is about.
type JobRef struct {
	ID        uuid.UUID
	AgentType string
	// Tools names the proposed calls, in order.
	Tools []string
}

// NewApprovalRequestedEvent asks the task's assignee, or its creator when
// unassigned, to decide on the job's proposals. The actor is the agent.
func NewApprovalRequestedEvent(t *Task, job *AgentJob, tools []string, now time.Time) Event {
	e := t.event(EventAgentApprovalRequested, Agent(job.AgentType), now.UTC())
	e.Job = &JobRef{ID: job.ID, AgentType: job.AgentType, Tools: slices.Clone(tools)}
	return e
}

// CanDecide reports whether userID may approve or reject proposals about
// the task: its assignee, its creator, or an admin (default G10).
func (t *Task) CanDecide(userID string, admin bool) bool {
	return admin || (userID != "" && (userID == t.AssigneeID || userID == t.CreatorID))
}
