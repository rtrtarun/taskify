package domain

import (
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxTitleLen     = 255
	maxTenantLen    = 64
	maxWorkspaceLen = 64
	maxUserIDLen    = 128
	maxLabels       = 50
	maxLabelLen     = 64
)

// Task is the aggregate root. Mutate it only through NewTask, Apply, and
// TransitionTo, so that validation, versioning, and events stay consistent.
type Task struct {
	ID          uuid.UUID
	TenantID    string
	WorkspaceID string
	Title       string
	Description string
	Status      Status
	Priority    Priority
	CreatorID   string
	AssigneeID  string // empty when unassigned
	Labels      []string
	Metadata    map[string]any
	Watchers    []string

	DueAt                 *time.Time
	RemindAt              *time.Time
	CompletedAt           *time.Time
	ArchivedAt            *time.Time
	LastDueSoonNotifiedAt *time.Time
	LastOverdueNotifiedAt *time.Time

	// Set exactly while Status is blocked.
	StatusBeforeBlocked *Status
	BlockedReason       *BlockedReason
	// UnblockedAt is when the task last left blocked for todo or
	// in_progress, by a person or by dependency release. The morning brief
	// lists the tasks unblocked since the last brief (M10-02).
	UnblockedAt *time.Time

	// EstimatedMinutes is the expected effort, when known.
	EstimatedMinutes *int
	// AgentMetadata is what agents recorded about the task.
	AgentMetadata AgentMetadata

	// ParentID is the task this one was split from, if any. It is set when
	// a task is created and never changed by an update; deleting the parent
	// clears it (departure D15).
	ParentID *uuid.UUID

	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewTaskInput is what a caller supplies to create a task.
type NewTaskInput struct {
	TenantID    string
	WorkspaceID string
	Title       string
	Description string
	Priority    Priority // defaults to p3_medium
	AssigneeID  string
	Labels      []string
	Metadata    map[string]any
	Watchers    []string
	DueAt       *time.Time
	RemindAt    *time.Time
}

// NewTask validates the input and returns a task in todo at version 1, with
// the events its creation produces. The actor becomes the creator.
//
// Creation raises task.created only, and only when the assignee is someone
// other than the creator. It does not also raise task.assigned: both would go
// to the same person for the same action.
func NewTask(in NewTaskInput, actor Actor, now time.Time) (*Task, []Event, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, nil, err
	}
	if in.Priority == "" {
		in.Priority = PriorityMedium
	}
	now = now.UTC()
	t := &Task{
		ID:          id,
		TenantID:    strings.TrimSpace(in.TenantID),
		WorkspaceID: strings.TrimSpace(in.WorkspaceID),
		Title:       strings.TrimSpace(in.Title),
		Description: in.Description,
		Status:      StatusTodo,
		Priority:    in.Priority,
		CreatorID:   strings.TrimSpace(actor.ID),
		AssigneeID:  strings.TrimSpace(in.AssigneeID),
		Labels:      normalizeList(in.Labels),
		Metadata:    maps.Clone(in.Metadata),
		Watchers:    normalizeSet(in.Watchers),
		DueAt:       utcPtr(in.DueAt),
		RemindAt:    utcPtr(in.RemindAt),
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if t.Metadata == nil {
		t.Metadata = map[string]any{}
	}
	if err := t.validate(); err != nil {
		return nil, nil, err
	}

	var events []Event
	if t.AssigneeID != "" && t.AssigneeID != t.CreatorID {
		events = append(events, t.event(EventTaskCreated, actor, now))
	}
	return t, events, nil
}

// TimePatch changes a nullable timestamp. Set with a nil Value clears it.
type TimePatch struct {
	Set   bool
	Value *time.Time
}

// SetTime returns a TimePatch that sets the field to v.
func SetTime(v time.Time) TimePatch { return TimePatch{Set: true, Value: &v} }

// ClearTime returns a TimePatch that clears the field.
func ClearTime() TimePatch { return TimePatch{Set: true} }

// Patch describes a partial update. A nil field is left unchanged.
type Patch struct {
	Title       *string
	Description *string
	Status      *Status
	Priority    *Priority
	AssigneeID  *string // a pointer to "" unassigns
	Labels      *[]string
	Metadata    *map[string]any
	Watchers    *[]string
	DueAt       TimePatch
	RemindAt    TimePatch
	// EstimatedMinutes sets the estimate; it must be positive.
	EstimatedMinutes *int

	// BlockedReason applies only when Status moves to blocked. It defaults
	// to manual; the system passes dependency.
	BlockedReason BlockedReason
}

// Apply validates and applies the patch, increments the version, and returns
// the events the change produces. On error the task is left untouched. A patch
// that changes nothing returns no events and does not increment the version.
func (t *Task) Apply(p Patch, actor Actor, now time.Time) ([]Event, error) {
	now = now.UTC()
	next := t.clone()
	prev := t // read-only view of the state before the change

	if p.Title != nil {
		next.Title = strings.TrimSpace(*p.Title)
	}
	if p.Description != nil {
		next.Description = *p.Description
	}
	if p.Priority != nil {
		next.Priority = *p.Priority
	}
	if p.AssigneeID != nil {
		next.AssigneeID = strings.TrimSpace(*p.AssigneeID)
	}
	if p.Labels != nil {
		next.Labels = normalizeList(*p.Labels)
	}
	if p.Metadata != nil {
		next.Metadata = maps.Clone(*p.Metadata)
		if next.Metadata == nil {
			next.Metadata = map[string]any{}
		}
	}
	if p.Watchers != nil {
		next.Watchers = normalizeSet(*p.Watchers)
	}
	if p.DueAt.Set {
		next.DueAt = utcPtr(p.DueAt.Value)
	}
	if p.RemindAt.Set {
		next.RemindAt = utcPtr(p.RemindAt.Value)
	}
	if p.EstimatedMinutes != nil {
		v := *p.EstimatedMinutes
		next.EstimatedMinutes = &v
	}
	if p.Status != nil && *p.Status != prev.Status {
		if err := next.moveTo(*p.Status, p.BlockedReason, now); err != nil {
			return nil, err
		}
	}
	if err := next.validate(); err != nil {
		return nil, err
	}

	dueChanged := !timeEqual(prev.DueAt, next.DueAt)
	contentChanged := prev.Title != next.Title || prev.Description != next.Description || dueChanged
	statusChanged := prev.Status != next.Status
	assigneeChanged := prev.AssigneeID != next.AssigneeID

	changed := contentChanged || statusChanged || assigneeChanged ||
		prev.Priority != next.Priority ||
		!slices.Equal(prev.Labels, next.Labels) ||
		!slices.Equal(prev.Watchers, next.Watchers) ||
		!timeEqual(prev.RemindAt, next.RemindAt) ||
		!intEqual(prev.EstimatedMinutes, next.EstimatedMinutes) ||
		p.Metadata != nil
	if !changed {
		return nil, nil
	}

	if dueChanged {
		// A rescheduled task must be reminded again.
		next.LastDueSoonNotifiedAt = nil
	}
	next.Version = prev.Version + 1
	next.UpdatedAt = now

	var events []Event
	add := func(eventType string) {
		e := next.event(eventType, actor, now)
		if statusChanged {
			e.PreviousStatus = prev.Status
		}
		events = append(events, e)
	}
	if assigneeChanged && next.AssigneeID != "" {
		add(EventTaskAssigned)
	}
	if next.Priority.HigherThan(prev.Priority) {
		add(EventTaskPriorityEscalated)
	}
	if statusChanged && next.Status == StatusBlocked {
		add(EventTaskBlocked)
	}
	if statusChanged && next.Status == StatusCompleted {
		add(EventTaskCompleted)
	}
	if contentChanged {
		add(EventTaskUpdated)
	}

	*t = next
	return events, nil
}

// TransitionTo changes only the status.
func (t *Task) TransitionTo(to Status, actor Actor, now time.Time) ([]Event, error) {
	return t.Apply(Patch{Status: &to}, actor, now)
}

// transitions is the matrix in docs/architecture.md section 4.3.
var transitions = map[Status][]Status{
	StatusTodo:       {StatusInProgress, StatusCompleted, StatusBlocked, StatusArchived},
	StatusInProgress: {StatusBlocked, StatusCompleted, StatusTodo, StatusArchived},
	StatusBlocked:    {StatusInProgress, StatusTodo, StatusArchived},
	StatusCompleted:  {StatusInProgress, StatusArchived},
	StatusArchived:   {StatusTodo},
}

// CanTransition reports whether the task may move to the given status. Moving
// to the current status is not a transition.
func (t *Task) CanTransition(to Status) bool {
	return slices.Contains(transitions[t.Status], to)
}

// IsOpen reports whether the task still needs work, which is what deadline
// scans look for.
func (t *Task) IsOpen() bool {
	return t.Status != StatusCompleted && t.Status != StatusArchived
}

// moveTo performs a status change and its side effects on the receiver.
func (t *Task) moveTo(to Status, reason BlockedReason, now time.Time) error {
	if !to.Valid() {
		return invalid("unknown status %q", to)
	}
	if !t.CanTransition(to) {
		return &TransitionError{From: t.Status, To: to}
	}
	from := t.Status
	t.Status = to

	// Block bookkeeping: both columns are set exactly while blocked.
	if to == StatusBlocked {
		if reason == "" {
			reason = BlockedManual
		}
		if !reason.Valid() {
			return invalid("unknown blocked reason %q", reason)
		}
		t.StatusBeforeBlocked = &from
		t.BlockedReason = &reason
	} else {
		t.StatusBeforeBlocked = nil
		t.BlockedReason = nil
	}
	if from == StatusBlocked && (to == StatusTodo || to == StatusInProgress) {
		t.UnblockedAt = &now
	}

	switch to {
	case StatusCompleted:
		t.CompletedAt = &now
	case StatusArchived:
		// An archived task keeps completed_at so its history survives.
		t.ArchivedAt = &now
	default:
		// Reopen and restore both return the task to open work.
		t.CompletedAt = nil
	}
	if to != StatusArchived {
		t.ArchivedAt = nil
	}
	return nil
}

// TransitionError is returned for a status change outside the matrix. It
// matches ErrInvalidStateTransition with errors.Is.
type TransitionError struct {
	From, To Status
}

func (e *TransitionError) Error() string {
	return "invalid state transition: " + string(e.From) + " to " + string(e.To)
}

func (e *TransitionError) Is(target error) bool { return target == ErrInvalidStateTransition }

func (t *Task) validate() error {
	switch {
	case t.TenantID == "":
		return invalid("tenant_id is required")
	case len(t.TenantID) > maxTenantLen:
		return invalid("tenant_id must be at most %d characters", maxTenantLen)
	case t.WorkspaceID == "":
		return invalid("workspace_id is required")
	case len(t.WorkspaceID) > maxWorkspaceLen:
		return invalid("workspace_id must be at most %d characters", maxWorkspaceLen)
	case t.Title == "":
		return invalid("title is required")
	case utf8.RuneCountInString(t.Title) > maxTitleLen:
		return invalid("title must be at most %d characters", maxTitleLen)
	case !t.Status.Valid():
		return invalid("unknown status %q", t.Status)
	case !t.Priority.Valid():
		return invalid("unknown priority %q", t.Priority)
	case t.CreatorID == "":
		return invalid("creator_id is required")
	case len(t.CreatorID) > maxUserIDLen:
		return invalid("creator_id must be at most %d characters", maxUserIDLen)
	case len(t.AssigneeID) > maxUserIDLen:
		return invalid("assignee_id must be at most %d characters", maxUserIDLen)
	case len(t.Labels) > maxLabels:
		return invalid("a task can have at most %d labels", maxLabels)
	case t.EstimatedMinutes != nil && (*t.EstimatedMinutes <= 0 || *t.EstimatedMinutes > maxEstimateMinutes):
		return invalid("estimated_minutes must be between 1 and %d", maxEstimateMinutes)
	}
	for _, l := range t.Labels {
		if len(l) > maxLabelLen {
			return invalid("a label must be at most %d characters", maxLabelLen)
		}
	}
	for _, w := range t.Watchers {
		if len(w) > maxUserIDLen {
			return invalid("a watcher ID must be at most %d characters", maxUserIDLen)
		}
	}
	return nil
}

// clone returns a copy that shares no slices or maps with the receiver.
func (t *Task) clone() Task {
	c := *t
	c.Labels = slices.Clone(t.Labels)
	c.Watchers = slices.Clone(t.Watchers)
	c.Metadata = maps.Clone(t.Metadata)
	c.AgentMetadata = t.AgentMetadata.Clone()
	if t.EstimatedMinutes != nil {
		v := *t.EstimatedMinutes
		c.EstimatedMinutes = &v
	}
	return c
}

// normalizeList trims entries, drops empty ones, and removes duplicates while
// keeping first-seen order. It never returns nil.
func normalizeList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// normalizeSet is normalizeList, sorted. Watchers are a set; sorting gives
// them one canonical order, the same order the database returns.
func normalizeSet(in []string) []string {
	out := normalizeList(in)
	slices.Sort(out)
	return out
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// maxEstimateMinutes bounds an estimate at 60 working days of 8 hours.
const maxEstimateMinutes = 60 * 8 * 60

func intEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func timeEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
