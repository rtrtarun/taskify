package domain

// Status is a task's position in its lifecycle.
type Status string

const (
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusCompleted  Status = "completed"
	StatusArchived   Status = "archived"
)

// Statuses lists every status, in lifecycle order.
var Statuses = []Status{StatusTodo, StatusInProgress, StatusBlocked, StatusCompleted, StatusArchived}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, v := range Statuses {
		if s == v {
			return true
		}
	}
	return false
}

// Priority is a task's urgency. p1_critical is the most urgent.
type Priority string

const (
	PriorityCritical Priority = "p1_critical"
	PriorityHigh     Priority = "p2_high"
	PriorityMedium   Priority = "p3_medium"
	PriorityLow      Priority = "p4_low"
)

// Priorities lists every priority, most urgent first.
var Priorities = []Priority{PriorityCritical, PriorityHigh, PriorityMedium, PriorityLow}

// Valid reports whether p is a known priority.
func (p Priority) Valid() bool { return p.rank() != 0 }

// rank is 1 for the most urgent priority and 4 for the least; 0 if unknown.
func (p Priority) rank() int {
	for i, v := range Priorities {
		if p == v {
			return i + 1
		}
	}
	return 0
}

// HigherThan reports whether p is more urgent than other.
func (p Priority) HigherThan(other Priority) bool {
	return p.Valid() && other.Valid() && p.rank() < other.rank()
}

// Hint maps a priority to AgentPulse's priorityHint vocabulary.
func (p Priority) Hint() string {
	switch p {
	case PriorityCritical:
		return "critical"
	case PriorityHigh:
		return "high"
	case PriorityLow:
		return "low"
	default:
		return "medium"
	}
}

// Label is the human-readable form used in notification text.
func (p Priority) Label() string {
	switch p {
	case PriorityCritical:
		return "P1 Critical"
	case PriorityHigh:
		return "P2 High"
	case PriorityLow:
		return "P4 Low"
	default:
		return "P3 Medium"
	}
}

// ActorKind says who caused a change. Only changes by a user may start agent
// work; this is what prevents agent feedback loops.
type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorAgent  ActorKind = "agent"
	ActorSystem ActorKind = "system"
)

// Actor is the originator of a change.
type Actor struct {
	ID   string
	Kind ActorKind
}

// User returns an Actor for a person.
func User(id string) Actor { return Actor{ID: id, Kind: ActorUser} }

// Agent returns the Actor for an agent of the given type, such as
// "agent:triage". Its changes notify people but never queue agent work.
func Agent(agentType string) Actor { return Actor{ID: "agent:" + agentType, Kind: ActorAgent} }

// System is the actor for changes made by Taskify itself, such as scheduler
// reminders.
var System = Actor{ID: "system", Kind: ActorSystem}

// BlockedReason says why a task is blocked, which decides who may unblock it.
type BlockedReason string

const (
	// BlockedManual is set by a person and is only lifted by a person.
	BlockedManual BlockedReason = "manual"
	// BlockedDependency is set by the system and lifted when the last
	// dependency resolves.
	BlockedDependency BlockedReason = "dependency"
)

// Valid reports whether r is a known reason.
func (r BlockedReason) Valid() bool { return r == BlockedManual || r == BlockedDependency }
