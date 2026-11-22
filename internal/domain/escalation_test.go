package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewEscalationEvents(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	blockedReason := BlockedManual
	before := StatusInProgress
	task := &Task{ID: uuid.New(), TenantID: "t1", WorkspaceID: "ws", Title: "Launch", Status: StatusBlocked,
		Priority: PriorityLow, AssigneeID: "bob", CreatorID: "alice", Watchers: []string{"wendy"}, Version: 3,
		BlockedReason: &blockedReason, StatusBeforeBlocked: &before}
	job := &AgentJob{ID: uuid.New(), AgentType: AgentBlocker}
	mk := func(title, assignee, creator string, s Status) *Task {
		return &Task{ID: uuid.New(), TenantID: "t1", Title: title, AssigneeID: assignee, CreatorID: creator, Status: s, Version: 1}
	}
	c1 := mk("Key", "carol", "alice", StatusTodo)
	d1 := mk("DNS", "", "dave", StatusInProgress)
	c2 := mk("Contract", "carol", "alice", StatusBlocked)
	done := mk("Spec", "erin", "alice", StatusCompleted)
	gone := mk("Old", "frank", "alice", StatusArchived)

	events := NewEscalationEvents(task, job, []*Task{c1, d1, done, c2, gone}, now)
	if len(events) != 2 {
		t.Fatalf("%d events, want carol and dave", len(events))
	}
	carol, dave := events[0], events[1]
	if r := carol.Recipients(); len(r) != 1 || r[0] != "carol" || len(carol.Escalation.Blockers) != 2 {
		t.Errorf("carol = %v %+v", r, carol.Escalation)
	}
	if r := dave.Recipients(); len(r) != 1 || r[0] != "dave" {
		t.Errorf("dave = %v", r)
	}
	if carol.Actor != Agent(AgentBlocker) || carol.Type != EventTaskEscalated {
		t.Errorf("event = %+v", carol)
	}

	ns := carol.Notifications(Links{API: "https://x"})
	if len(ns) != 1 {
		t.Fatalf("notifications = %+v", ns)
	}
	n := ns[0]
	wantKey := "taskify:t1:" + task.ID.String() + ":task.escalated:" + job.ID.String() + ":carol"
	if n.IdempotencyKey != wantKey || n.UserID != "carol" || n.EventType != EventTaskEscalated ||
		n.PriorityHint != "high" || len(n.Channels) != 3 || n.Payload["bypassDigest"] != true ||
		n.Payload["blockedReason"] != "manual" || n.Payload["jobId"] != job.ID.String() {
		t.Errorf("notification = %+v", n)
	}
	if !strings.HasPrefix(n.Title, "[ESCALATED]") || !strings.Contains(n.Body, `"Key"`) || !strings.Contains(n.Body, `"Contract"`) ||
		!strings.Contains(n.Body, ActionUnblockLabel) {
		t.Errorf("title %q body %q", n.Title, n.Body)
	}
	urls := n.Payload["actionUrls"].(map[string]string)
	// D50: no view without APP_BASE_URL.
	if urls[ActionUnblock] != "https://x/api/v1/tasks/"+task.ID.String() || urls["approve"] != "" || len(urls) != 1 {
		t.Errorf("actionUrls = %v", urls)
	}
	if dave.IdempotencyKey("dave") == carol.IdempotencyKey("carol") {
		t.Error("two recipients share a key")
	}

	// A task that is not blocked offers no unblock action, and without
	// APP_BASE_URL nothing at all (D50).
	task.Status, task.BlockedReason, task.StatusBeforeBlocked = StatusInProgress, nil, nil
	n = NewEscalationEvents(task, job, []*Task{c1}, now)[0].Notifications(Links{API: "https://x"})[0]
	if _, ok := n.Payload["actionUrls"]; ok || n.Payload["actionLabels"] != nil ||
		strings.Contains(n.Body, ActionUnblockLabel) {
		t.Errorf("in-progress escalation = %+v", n)
	}
	// Nothing open, nobody to ask.
	if got := NewEscalationEvents(task, job, []*Task{done, gone}, now); len(got) != 0 {
		t.Errorf("resolved blockers escalated: %v", got)
	}
}

// TestEscalationViewWithTheApp: with APP_BASE_URL, an escalation views the
// escalated task in the app, next to unblock while it is blocked (AP-10).
func TestEscalationViewWithTheApp(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	task, _, err := NewTask(NewTaskInput{TenantID: "t1", WorkspaceID: "ws", Title: "Ship it", AssigneeID: "bob"}, User("alice"), now)
	if err != nil {
		t.Fatal(err)
	}
	blocker, _, err := NewTask(NewTaskInput{TenantID: "t1", WorkspaceID: "ws", Title: "Key", AssigneeID: "carol"}, User("alice"), now)
	if err != nil {
		t.Fatal(err)
	}
	job := uuid.New()
	links := Links{API: "https://x", App: "https://app"}
	page := "https://app/tasks/" + task.ID.String()

	task.Status = StatusInProgress
	n := NewEscalationEvents(task, &AgentJob{ID: job, AgentType: AgentBlocker}, []*Task{blocker}, now)[0].Notifications(links)[0]
	if urls := n.Payload["actionUrls"].(map[string]string); urls[ActionView] != page || len(urls) != 1 {
		t.Errorf("in-progress escalation actionUrls = %v", urls)
	}
	manual := BlockedManual
	prev := StatusInProgress
	task.Status, task.BlockedReason, task.StatusBeforeBlocked = StatusBlocked, &manual, &prev
	n = NewEscalationEvents(task, &AgentJob{ID: job, AgentType: AgentBlocker}, []*Task{blocker}, now)[0].Notifications(links)[0]
	if urls := n.Payload["actionUrls"].(map[string]string); urls[ActionView] != page || urls[ActionUnblock] == "" || len(urls) != 2 {
		t.Errorf("blocked escalation actionUrls = %v", urls)
	}
}
