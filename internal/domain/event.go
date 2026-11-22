package domain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The nine R1 event types. See docs/architecture.md section 5.6. The R2
// dependency events are in dependency.go, agent.approval_requested in
// approval.go, and task.escalated in escalation.go.
const (
	EventTaskCreated           = "task.created"
	EventTaskAssigned          = "task.assigned"
	EventTaskUpdated           = "task.updated"
	EventTaskPriorityEscalated = "task.priority_escalated"
	EventTaskBlocked           = "task.blocked"
	EventTaskDueSoon           = "task.due_soon"
	EventTaskOverdue           = "task.overdue"
	EventTaskCompleted         = "task.completed"
	EventTaskCommentAdded      = "task.comment_added"
)

// Due-soon reminder windows.
const (
	DueWindow24h = "24h"
	DueWindow1h  = "1h"
)

// Source identifies Taskify to AgentPulse.
const Source = "taskify"

// The payload.actionUrls keys AgentPulse accepts (docs/blockers.md, "Agreed
// with AgentPulse"); ActionUnblock, in escalation.go, is the sixth. A
// notification uses no other key. AgentPulse makes its own signed link for
// every key but view, which it uses as sent, and a click reaches the webhook
// as the callback action of the same name (snooze_2h as snooze with duration
// 2h). The callback's ID is read from the payload: taskId for complete,
// snooze_2h, and unblock, jobId for approve and reject.
const (
	// ActionComplete is the "Mark done" button.
	ActionComplete = "complete"
	ActionSnooze2h = "snooze_2h"
	ActionApprove  = "approve"
	ActionReject   = "reject"
	// ActionView is a plain link, used as sent.
	ActionView = "view"
)

// Event is something that happened to a task. It carries a snapshot of the
// task after the change, so a notification can be built from the event alone.
type Event struct {
	Type       string
	Actor      Actor
	Task       Task
	OccurredAt time.Time

	// PreviousStatus is set when the change moved the task to a new status.
	PreviousStatus Status
	// Comment is set for task.comment_added.
	Comment *Comment
	// DueWindow is set for task.due_soon: DueWindow24h or DueWindow1h.
	DueWindow string
	// Blocker is set for dependency events and system-applied blocks: the
	// task whose change caused this one.
	Blocker *BlockerRef
	// Job is set for agent.approval_requested.
	Job *JobRef
	// Escalation is set for task.escalated.
	Escalation *Escalation
	// Deferred holds the notification for the recipient's digest: the load
	// gatekeeper sets it on an assignment to an overloaded assignee (M10-01,
	// G14). Only a Deferrable event is deferred.
	Deferred bool
}

// Digest routing (decision 5, feature A4).
const (
	// ChannelDigest, alone in channels, delivers a notification only in the
	// recipient's digest.
	ChannelDigest = "digest"
	// DeferredDigestSummary ends the digest line of a deferred notification,
	// which DeferredDigestLine builds.
	DeferredDigestSummary = "Deferred to avoid context switching"
)

// DeferredDigestLine is what the digest shows for an assignment held there
// (AP-07): "Assigned to you: {title}. Deferred to avoid context switching".
func DeferredDigestLine(title string) string {
	return "Assigned to you: " + title + ". " + DeferredDigestSummary
}

// Deferrable reports whether the load gatekeeper may hold this event's
// notification for the digest: an assignment (task.created with an
// assignee, which stands for the assignment since D1, or task.assigned)
// that notifies the assignee and would not bypass the digest anyway. A
// critical task keeps bypassDigest and is never deferred.
func (e Event) Deferrable() bool {
	if e.Type != EventTaskCreated && e.Type != EventTaskAssigned {
		return false
	}
	return e.Task.AssigneeID != "" && len(e.Recipients()) > 0 && !e.bypassDigest()
}

func (t *Task) event(eventType string, actor Actor, at time.Time) Event {
	return Event{Type: eventType, Actor: actor, Task: t.clone(), OccurredAt: at}
}

// NewDueSoonEvent is raised by the scheduler ahead of a deadline.
func NewDueSoonEvent(t *Task, window string, now time.Time) Event {
	e := t.event(EventTaskDueSoon, System, now.UTC())
	e.DueWindow = window
	return e
}

// NewOverdueEvent is raised by the scheduler, at most once per task per day.
func NewOverdueEvent(t *Task, now time.Time) Event {
	return t.event(EventTaskOverdue, System, now.UTC())
}

// Recipients returns the users to notify, without duplicates and without the
// actor: nobody is told about their own action.
func (e Event) Recipients() []string {
	t := e.Task
	var candidates []string
	switch e.Type {
	case EventTaskCreated, EventTaskAssigned, EventTaskDueSoon, EventTaskOverdue,
		EventTaskUnblocked, EventTaskDependencyResolved, EventTaskDependencyAdded, EventTaskDependencyReopened:
		candidates = []string{t.AssigneeID}
	case EventTaskUpdated, EventTaskPriorityEscalated, EventTaskBlocked:
		candidates = append([]string{t.AssigneeID}, t.Watchers...)
	case EventTaskCompleted:
		candidates = append([]string{t.CreatorID}, t.Watchers...)
	case EventAgentApprovalRequested:
		// Someone must decide: the assignee, or the creator when unassigned.
		candidates = []string{t.AssigneeID}
		if t.AssigneeID == "" {
			candidates = []string{t.CreatorID}
		}
	case EventTaskCommentAdded:
		candidates = append([]string{t.AssigneeID}, t.Watchers...)
		if e.Comment != nil {
			candidates = append(candidates, e.Comment.Mentions...)
		}
	case EventTaskEscalated:
		// The owner of the blocking tasks, not the escalated task's people.
		if e.Escalation != nil {
			candidates = []string{e.Escalation.Recipient}
		}
	}
	out := []string{}
	for _, c := range candidates {
		if c == "" || slices.Contains(out, c) {
			continue
		}
		if e.Actor.Kind == ActorUser && c == e.Actor.ID {
			continue
		}
		out = append(out, c)
	}
	return out
}

// IdempotencyKey returns the deterministic key for this event and recipient.
// The recipient is appended only when the event has several, as in
// docs/architecture.md section 4.4.
//
// Two formats extend that section:
//   - due-soon keys end with the due time, so a rescheduled task is reminded
//     again;
//   - comment keys use the comment ID, because a comment does not change the
//     task version and two comments would otherwise share a key.
//   - dependency notices (added, resolved, reopened) use the blocker's ID and
//     version, for the same reason: they leave the dependent's version alone.
//   - escalations use the job ID and always the recipient: a job escalates
//     to each owner at most once.
func (e Event) IdempotencyKey(recipient string) string {
	base := fmt.Sprintf("%s:%s:%s", Source, e.Task.TenantID, e.Task.ID)
	var key string
	switch e.Type {
	case EventTaskDueSoon:
		due := int64(0)
		if e.Task.DueAt != nil {
			due = e.Task.DueAt.Unix()
		}
		key = fmt.Sprintf("%s:due_soon_%s:%d", base, e.DueWindow, due)
	case EventTaskOverdue:
		key = fmt.Sprintf("%s:overdue:%s", base, e.OccurredAt.UTC().Format("2006-01-02"))
	case EventTaskCommentAdded:
		id := ""
		if e.Comment != nil {
			id = e.Comment.ID.String()
		}
		key = fmt.Sprintf("%s:%s:%s", base, e.Type, id)
	case EventTaskDependencyAdded, EventTaskDependencyResolved, EventTaskDependencyReopened:
		var blocker string
		version := 0
		if e.Blocker != nil {
			blocker, version = e.Blocker.ID.String(), e.Blocker.Version
		}
		key = fmt.Sprintf("%s:%s:%s:v%d", base, e.Type, blocker, version)
	case EventAgentApprovalRequested:
		// A job asks for approval at most once.
		job := ""
		if e.Job != nil {
			job = e.Job.ID.String()
		}
		key = fmt.Sprintf("%s:%s:%s", base, e.Type, job)
	case EventTaskEscalated:
		job := ""
		if e.Escalation != nil {
			job = e.Escalation.JobID.String()
		}
		return fmt.Sprintf("%s:%s:%s:%s", base, e.Type, job, recipient)
	default:
		key = fmt.Sprintf("%s:%s:v%d", base, e.Type, e.Task.Version)
	}
	if len(e.Recipients()) > 1 {
		key += ":" + recipient
	}
	return key
}

// Notification is the body of an AgentPulse /notify request. Field names are
// camelCase, as AgentPulse expects.
type Notification struct {
	IdempotencyKey string         `json:"idempotencyKey"`
	TenantID       string         `json:"tenantId"`
	UserID         string         `json:"userId"`
	Source         string         `json:"source"`
	EventType      string         `json:"eventType"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	PriorityHint   string         `json:"priorityHint"`
	Channels       []string       `json:"channels,omitempty"`
	Payload        map[string]any `json:"payload"`
}

// immediateChannels is used with bypassDigest, and ChannelDigest alone for a
// deferred notification. Otherwise channels are left to AgentPulse's rules
// and preferences.
var immediateChannels = []string{"slack", "email", "in_app"}

// Notifications builds one /notify request per recipient. links are the
// addresses its action and view links are built from.
func (e Event) Notifications(links Links) []Notification {
	recipients := e.Recipients()
	out := make([]Notification, 0, len(recipients))
	for _, r := range recipients {
		payload := e.payload(links)
		mentioned := e.Comment != nil && slices.Contains(e.Comment.Mentions, r)
		if e.Type == EventTaskCommentAdded {
			payload["isMentioned"] = mentioned
		}
		n := Notification{
			IdempotencyKey: e.IdempotencyKey(r),
			TenantID:       e.Task.TenantID,
			UserID:         r,
			Source:         Source,
			EventType:      e.Type,
			Title:          e.title(mentioned),
			Body:           e.body(),
			PriorityHint:   e.priorityHint(),
			Payload:        payload,
		}
		switch {
		case e.bypassDigest():
			n.Channels = slices.Clone(immediateChannels)
		case e.Deferred && e.Deferrable():
			n.Channels = []string{ChannelDigest}
			payload["digestSummary"] = DeferredDigestLine(e.Task.Title)
		}
		out = append(out, n)
	}
	return out
}

// bypassDigest forces immediate delivery for critical tasks, manual blocks,
// and escalations. A system-applied dependency block is deliberately quiet.
func (e Event) bypassDigest() bool {
	if e.Task.Priority == PriorityCritical || e.Type == EventTaskEscalated {
		return true
	}
	return e.Type == EventTaskBlocked && e.blockedReason() == BlockedManual
}

func (e Event) blockedReason() BlockedReason {
	if e.Task.BlockedReason == nil {
		return ""
	}
	return *e.Task.BlockedReason
}

func (e Event) priorityHint() string {
	// A manual block is at least high, whatever the task's own priority.
	// A revived blocker on started work is sent as at least high (section 5.6),
	// and so is an escalation.
	// An agent's priority recommendation, proposed for approval, is the hint
	// for the task's later notifications (M7-04).
	manualBlock := e.Type == EventTaskBlocked && e.blockedReason() == BlockedManual
	p := e.Task.Priority
	if rec := e.Task.AgentMetadata.PriorityRecommendation; rec != nil && rec.Value.Valid() {
		p = rec.Value
	}
	escalated := manualBlock || e.Type == EventTaskDependencyReopened || e.Type == EventTaskEscalated
	if escalated && PriorityHigh.HigherThan(p) {
		p = PriorityHigh
	}
	return p.Hint()
}

func (e Event) payload(links Links) map[string]any {
	t := e.Task
	taskURL := links.taskAPI(t.ID)
	// view opens the task in the web app. Without APP_BASE_URL there is no
	// page to open, and a task notification carries no view link (AP-10).
	view := links.TaskView(t.ID)
	p := map[string]any{
		"taskId":      t.ID.String(),
		"workspaceId": t.WorkspaceID,
		"status":      string(t.Status),
		"priority":    string(t.Priority),
		"labels":      append([]string{}, t.Labels...),
		"actorId":     e.Actor.ID,
		"actorKind":   string(e.Actor.Kind),
		"version":     t.Version,
	}
	urls := map[string]string{}
	if t.IsOpen() {
		// "Mark done" and "Snooze 2h" while there is work left to do. The
		// URLs name the matching Taskify endpoints; AgentPulse sends its own
		// signed links and calls the webhook with taskId.
		urls[ActionComplete] = taskURL + "/complete"
		urls[ActionSnooze2h] = taskURL + "/feedback?signal=snoozed&duration=2h"
	}
	if view != "" {
		urls[ActionView] = view
	}
	if len(urls) > 0 {
		p["actionUrls"] = urls
	}
	if e.PreviousStatus != "" {
		p["previousStatus"] = string(e.PreviousStatus)
	}
	if t.DueAt != nil {
		p["dueAt"] = t.DueAt.UTC().Format(time.RFC3339)
	}
	if (e.Type == EventTaskBlocked || e.Type == EventTaskEscalated) && e.blockedReason() != "" {
		p["blockedReason"] = string(e.blockedReason())
	}
	if e.Type == EventTaskDueSoon {
		p["dueWindow"] = e.DueWindow
	}
	if e.Comment != nil {
		p["commentId"] = e.Comment.ID.String()
	}
	if e.Blocker != nil {
		p["blockerTaskId"] = e.Blocker.ID.String()
		p["blockerStatus"] = string(e.Blocker.Status)
	}
	if j := e.Job; j != nil {
		jobURL := links.jobAPI(j.ID)
		p["jobId"] = j.ID.String()
		p["agentType"] = j.AgentType
		p["proposedTools"] = append([]string{}, j.Tools...)
		// view opens the task in the web app, and is left out without it:
		// the job's API URL needs a bearer token a click cannot carry (D50).
		approval := map[string]string{
			ActionApprove: jobURL + "/apply",
			ActionReject:  jobURL + "/dismiss",
		}
		if view != "" {
			approval[ActionView] = view
		}
		p["actionUrls"] = approval
	}
	if x := e.Escalation; x != nil {
		p["jobId"] = x.JobID.String()
		p["agentType"] = x.AgentType
		blocking := make([]map[string]any, len(x.Blockers))
		for i, b := range x.Blockers {
			blocking[i] = map[string]any{"taskId": b.ID.String(), "title": b.Title, "status": string(b.Status)}
		}
		p["blockingTasks"] = blocking
		// view opens the escalated task in the web app, and is left out
		// without it (D50). An escalation about a task that is not blocked
		// then carries no actionUrls at all.
		escalation := map[string]string{}
		if view != "" {
			escalation[ActionView] = view
		}
		if t.Status == StatusBlocked {
			// The click reaches the webhook as action "unblock" for taskId.
			escalation[ActionUnblock] = taskURL
			p["actionLabels"] = map[string]string{ActionUnblock: ActionUnblockLabel}
		}
		if len(escalation) > 0 {
			p["actionUrls"] = escalation
		} else {
			delete(p, "actionUrls")
		}
	}
	if e.bypassDigest() {
		p["bypassDigest"] = true
	}
	return p
}

func (e Event) title(mentioned bool) string {
	t := e.Task.Title
	switch e.Type {
	case EventTaskCreated:
		return "New task: " + t
	case EventTaskAssigned:
		return "Assigned to you: " + t
	case EventTaskUpdated:
		return "Task updated: " + t
	case EventTaskPriorityEscalated:
		return "Priority raised to " + e.Task.Priority.Label() + ": " + t
	case EventTaskBlocked:
		return "[BLOCKED] " + t
	case EventTaskDueSoon:
		if e.DueWindow == DueWindow1h {
			return "Due in 1 hour: " + t
		}
		return "Due in 24 hours: " + t
	case EventTaskOverdue:
		return "Overdue: " + t
	case EventTaskCompleted:
		return "Completed: " + t
	case EventTaskCommentAdded:
		if mentioned {
			return "You were mentioned on: " + t
		}
		return "New comment on: " + t
	case EventTaskUnblocked:
		return "Unblocked: " + t
	case EventTaskDependencyResolved:
		return "Dependency done: " + t
	case EventTaskDependencyAdded:
		return "New dependency: " + t
	case EventTaskDependencyReopened:
		return "Dependency reopened: " + t
	case EventAgentApprovalRequested:
		return "Approval needed: " + t
	case EventTaskEscalated:
		return "[ESCALATED] Waiting on you: " + t
	}
	return t
}

func (e Event) body() string {
	ref := "Task #" + e.Task.ID.String()[:8]
	prio := "Priority: " + e.Task.Priority.Label() + "."
	by := e.Actor.ID
	switch e.Type {
	case EventTaskCreated:
		return fmt.Sprintf("%s was created by %s and assigned to you. %s", ref, by, prio)
	case EventTaskAssigned:
		return fmt.Sprintf("%s was assigned to you by %s. %s", ref, by, prio)
	case EventTaskUpdated:
		return fmt.Sprintf("%s was updated by %s. %s", ref, by, prio)
	case EventTaskPriorityEscalated:
		return fmt.Sprintf("%s was escalated by %s. %s", ref, by, prio)
	case EventTaskBlocked:
		return fmt.Sprintf("%s was marked as BLOCKED by %s. %s", ref, by, prio)
	case EventTaskDueSoon:
		return fmt.Sprintf("%s is due at %s. %s", ref, formatDue(e.Task.DueAt), prio)
	case EventTaskOverdue:
		return fmt.Sprintf("%s was due at %s and is still open. %s", ref, formatDue(e.Task.DueAt), prio)
	case EventTaskCompleted:
		return fmt.Sprintf("%s was completed by %s.", ref, by)
	case EventTaskCommentAdded:
		if e.Comment != nil {
			return fmt.Sprintf("%s commented: %s", by, truncate(e.Comment.Body, 280))
		}
	case EventAgentApprovalRequested:
		if j := e.Job; j != nil {
			return fmt.Sprintf("The %s agent proposes %s to %s: %s. Nothing changes until you approve. %s",
				j.AgentType, plural(len(j.Tools), "change"), ref, strings.Join(j.Tools, ", "), prio)
		}
	}
	if x := e.Escalation; x != nil && e.Type == EventTaskEscalated {
		names := make([]string, len(x.Blockers))
		for i, b := range x.Blockers {
			names[i] = fmt.Sprintf("%q (#%s)", truncate(b.Title, 80), b.ID.String()[:8])
		}
		state := "is " + string(e.Task.Status)
		next := "Please clear it or say what is needed."
		if e.Task.Status == StatusBlocked {
			next = "When it can go ahead, use " + ActionUnblockLabel + "."
		}
		return fmt.Sprintf("%s %s and waits on your %s %s. The %s agent investigated it. %s %s",
			ref, state, pluralWord(len(names), "task"), strings.Join(names, ", "), x.AgentType, next, prio)
	}
	if b := e.Blocker; b != nil {
		blocker := fmt.Sprintf("%q (#%s)", truncate(b.Title, 80), b.ID.String()[:8])
		switch e.Type {
		case EventTaskUnblocked:
			return fmt.Sprintf("%s is ready: its last dependency, %s, is %s. %s", ref, blocker, b.Status, prio)
		case EventTaskDependencyResolved:
			if e.Task.Status == StatusBlocked {
				return fmt.Sprintf("%s: dependency %s is %s. The task stays blocked until someone unblocks it. %s", ref, blocker, b.Status, prio)
			}
			return fmt.Sprintf("%s: dependency %s is %s. %s", ref, blocker, b.Status, prio)
		case EventTaskDependencyAdded:
			return fmt.Sprintf("%s now depends on %s, which is not done. Its status was left unchanged. %s", ref, blocker, prio)
		case EventTaskDependencyReopened:
			return fmt.Sprintf("%s: dependency %s is open again (%s). Its status was left unchanged. %s", ref, blocker, b.Status, prio)
		}
	}
	return ref
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func pluralWord(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func formatDue(t *time.Time) string {
	if t == nil {
		return "an unknown time"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "..."
}

// String makes an event readable in logs and test failures.
func (e Event) String() string {
	return e.Type + " v" + strconv.Itoa(e.Task.Version)
}
