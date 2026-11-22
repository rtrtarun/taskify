package domain

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

// EventTaskEscalated is sent by the blocker investigator to the owner of an
// open task that a blocked or stale task depends on (feature A2). It is its
// own event type, not task.blocked with a flag, so AgentPulse rules can tell
// the two apart (owner decision on G13).
const EventTaskEscalated = "task.escalated"

// ActionUnblock is the escalation's action: the webhook moves the escalated
// task from blocked to in_progress (default G7).
const ActionUnblock = "unblock"

// ActionUnblockLabel is the action's label in the notification.
const ActionUnblockLabel = "Unblock and set In Progress"

// Escalation is the part of a task.escalated event that the task snapshot
// does not hold: who is asked to help and which of their tasks are in the
// way.
type Escalation struct {
	JobID     uuid.UUID
	AgentType string
	// Recipient owns every task in Blockers.
	Recipient string
	// Blockers are the recipient's open tasks the escalated task depends on.
	Blockers []BlockerRef
}

// Owner is who answers for a task: its assignee, or its creator when it is
// unassigned.
func (t *Task) Owner() string {
	if t.AssigneeID != "" {
		return t.AssigneeID
	}
	return t.CreatorID
}

// NewEscalationEvents asks the owner of each open blocker of t for help: one
// event per distinct owner, naming all of that owner's blockers, in the order
// the blockers are given. Resolved blockers are ignored, so a task with no
// open blocker gets no escalation. The actor is the job's agent.
func NewEscalationEvents(t *Task, job *AgentJob, blockers []*Task, now time.Time) []Event {
	var (
		owners []string
		refs   = map[string][]BlockerRef{}
	)
	for _, b := range blockers {
		if b.ResolvesDependencies() {
			continue
		}
		o := b.Owner()
		if o == "" {
			continue
		}
		if !slices.Contains(owners, o) {
			owners = append(owners, o)
		}
		refs[o] = append(refs[o], *refOf(b))
	}
	out := make([]Event, 0, len(owners))
	for _, o := range owners {
		e := t.event(EventTaskEscalated, Agent(job.AgentType), now.UTC())
		e.Escalation = &Escalation{JobID: job.ID, AgentType: job.AgentType, Recipient: o, Blockers: refs[o]}
		out = append(out, e)
	}
	return out
}
