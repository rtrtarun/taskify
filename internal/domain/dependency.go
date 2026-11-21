package domain

import (
	"time"

	"github.com/google/uuid"
)

// R2 dependency event types (section 5.6).
const (
	// EventTaskUnblocked: a dependency-blocked task returned to its earlier
	// status because all its dependencies resolved.
	EventTaskUnblocked = "task.unblocked"
	// EventTaskDependencyResolved: a dependency resolved but the task was not
	// unblocked, because it is blocked manually or was never blocked.
	EventTaskDependencyResolved = "task.dependency_resolved"
	// EventTaskDependencyAdded: an unresolved dependency was added to a task
	// already in progress, so its status was left alone.
	EventTaskDependencyAdded = "task.dependency_added"
	// EventTaskDependencyReopened: a resolved dependency became open again for
	// a task already started or completed.
	EventTaskDependencyReopened = "task.dependency_reopened"
)

// Dependency means TaskID cannot start until DependsOnTaskID is completed or
// archived.
type Dependency struct {
	TaskID          uuid.UUID
	DependsOnTaskID uuid.UUID
	TenantID        string
	CreatedAt       time.Time
}

// NewDependency validates a link from dependent to blocker. Cycle detection
// needs the rest of the graph and is done by the service.
func NewDependency(dependent, blocker *Task, now time.Time) (Dependency, error) {
	switch {
	case dependent.ID == blocker.ID:
		return Dependency{}, invalid("a task cannot depend on itself")
	case dependent.TenantID != blocker.TenantID:
		// Callers load both tasks within one tenant, so this means a bug.
		return Dependency{}, invalid("a task cannot depend on a task in another tenant")
	}
	return Dependency{
		TaskID:          dependent.ID,
		DependsOnTaskID: blocker.ID,
		TenantID:        dependent.TenantID,
		CreatedAt:       now.UTC(),
	}, nil
}

// ResolvesDependencies reports whether the task, as a blocker, no longer holds
// up its dependents (decision 17).
func (t *Task) ResolvesDependencies() bool {
	return t.Status == StatusCompleted || t.Status == StatusArchived
}

// BlockerDeleted describes a blocker that was permanently deleted. It appears
// only in a BlockerRef, never as a task's status.
const BlockerDeleted Status = "deleted"

// BlockerRef identifies the task a dependency event is about.
type BlockerRef struct {
	ID      uuid.UUID
	Title   string
	Status  Status
	Version int
}

func refOf(blocker *Task) *BlockerRef {
	return &BlockerRef{ID: blocker.ID, Title: blocker.Title, Status: blocker.Status, Version: blocker.Version}
}

// NewDependencyEvent builds a dependency event for dependent, caused by a
// change to blocker. The actor is always the system: these events must never
// start agent work.
func NewDependencyEvent(eventType string, dependent, blocker *Task, now time.Time) Event {
	e := dependent.event(eventType, System, now.UTC())
	e.Blocker = refOf(blocker)
	return e
}

// ApplyDependencyBlock blocks a todo task because of an unresolved dependency.
// It is a system change and does nothing for a task in any other status.
func (t *Task) ApplyDependencyBlock(blocker *Task, now time.Time) ([]Event, error) {
	if t.Status != StatusTodo {
		return nil, nil
	}
	to := StatusBlocked
	events, err := t.Apply(Patch{Status: &to, BlockedReason: BlockedDependency}, System, now)
	for i := range events {
		events[i].Blocker = refOf(blocker)
	}
	return events, err
}

// LiftDependencyBlock returns a dependency-blocked task to the status it had
// before, with task.unblocked. It does nothing for any other task: a manual
// block is lifted by a person only.
func (t *Task) LiftDependencyBlock(blocker *Task, now time.Time) ([]Event, error) {
	if t.Status != StatusBlocked || t.BlockedReason == nil || *t.BlockedReason != BlockedDependency {
		return nil, nil
	}
	prev := t.Status
	to := StatusTodo
	if t.StatusBeforeBlocked != nil {
		to = *t.StatusBeforeBlocked
	}
	if _, err := t.Apply(Patch{Status: &to}, System, now); err != nil {
		return nil, err
	}
	e := NewDependencyEvent(EventTaskUnblocked, t, blocker, now)
	e.PreviousStatus = prev
	return []Event{e}, nil
}

// NewSystemComment returns a comment written by Taskify itself. It raises no
// task.comment_added event: the dependency event sent with it already tells
// the assignee, and a second notification would repeat it.
func NewSystemComment(t *Task, body string, now time.Time) (Comment, error) {
	c, _, err := NewComment(t, body, System, now)
	return c, err
}
