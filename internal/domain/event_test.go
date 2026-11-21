package domain

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const baseURL = "https://taskify.internal"

func TestRecipients(t *testing.T) {
	// The task is created by alice, assigned to bob, watched by carol.
	cases := []struct {
		eventType string
		actor     Actor
		want      []string
	}{
		{EventTaskCreated, alice, []string{"usr_bob"}},
		{EventTaskAssigned, alice, []string{"usr_bob"}},
		{EventTaskUpdated, alice, []string{"usr_bob", "usr_carol"}},
		{EventTaskPriorityEscalated, alice, []string{"usr_bob", "usr_carol"}},
		{EventTaskBlocked, alice, []string{"usr_bob", "usr_carol"}},
		{EventTaskDueSoon, System, []string{"usr_bob"}},
		{EventTaskOverdue, System, []string{"usr_bob"}},
		{EventTaskCompleted, bob, []string{"usr_alice", "usr_carol"}},
		// The actor is never notified of their own action.
		{EventTaskUpdated, bob, []string{"usr_carol"}},
		{EventTaskCompleted, alice, []string{"usr_carol"}},
		{EventTaskAssigned, bob, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.eventType+"_by_"+tc.actor.ID, func(t *testing.T) {
			e := newTask(t).event(tc.eventType, tc.actor, t0)
			if got := e.Recipients(); !slices.Equal(got, tc.want) {
				t.Errorf("Recipients = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("duplicates collapse", func(t *testing.T) {
		task := newTask(t)
		task.Watchers = []string{"usr_bob", "usr_carol", "usr_carol"}
		e := task.event(EventTaskUpdated, alice, t0)
		if got := e.Recipients(); !slices.Equal(got, []string{"usr_bob", "usr_carol"}) {
			t.Errorf("Recipients = %v", got)
		}
	})

	t.Run("an agent with a user's ID is not treated as that user", func(t *testing.T) {
		e := newTask(t).event(EventTaskUpdated, Actor{ID: "usr_bob", Kind: ActorAgent}, t0)
		if got := e.Recipients(); !slices.Contains(got, "usr_bob") {
			t.Errorf("Recipients = %v, want usr_bob included", got)
		}
	})
}

func TestIdempotencyKeys(t *testing.T) {
	task := newTask(t)
	id := task.ID.String()
	due := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	task.DueAt = &due

	t.Run("mutation with one recipient", func(t *testing.T) {
		e := task.event(EventTaskAssigned, alice, t0)
		want := "taskify:tenant_acme:" + id + ":task.assigned:v1"
		if got := e.IdempotencyKey("usr_bob"); got != want {
			t.Errorf("key = %q, want %q", got, want)
		}
	})

	t.Run("mutation with several recipients appends the user", func(t *testing.T) {
		e := task.event(EventTaskBlocked, alice, t0)
		want := "taskify:tenant_acme:" + id + ":task.blocked:v1:usr_carol"
		if got := e.IdempotencyKey("usr_carol"); got != want {
			t.Errorf("key = %q, want %q", got, want)
		}
	})

	t.Run("due soon includes the window and the due time", func(t *testing.T) {
		e24 := NewDueSoonEvent(task, DueWindow24h, t0)
		e1 := NewDueSoonEvent(task, DueWindow1h, t0)
		want := "taskify:tenant_acme:" + id + ":due_soon_24h:1794492000"
		if got := e24.IdempotencyKey("usr_bob"); got != want {
			t.Errorf("key = %q, want %q", got, want)
		}
		if e24.IdempotencyKey("usr_bob") == e1.IdempotencyKey("usr_bob") {
			t.Error("the 24h and 1h windows share a key")
		}

		moved := task.clone()
		later := due.Add(24 * time.Hour)
		moved.DueAt = &later
		if NewDueSoonEvent(&moved, DueWindow24h, t0).IdempotencyKey("usr_bob") == e24.IdempotencyKey("usr_bob") {
			t.Error("a rescheduled task reuses the old due-soon key")
		}
	})

	t.Run("overdue is keyed by UTC date", func(t *testing.T) {
		morning := NewOverdueEvent(task, time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC))
		evening := NewOverdueEvent(task, time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC))
		nextDay := NewOverdueEvent(task, time.Date(2026, 11, 14, 0, 30, 0, 0, time.UTC))
		want := "taskify:tenant_acme:" + id + ":overdue:2026-11-13"
		if got := morning.IdempotencyKey("usr_bob"); got != want {
			t.Errorf("key = %q, want %q", got, want)
		}
		if morning.IdempotencyKey("usr_bob") != evening.IdempotencyKey("usr_bob") {
			t.Error("two scans on one day produce different keys")
		}
		if morning.IdempotencyKey("usr_bob") == nextDay.IdempotencyKey("usr_bob") {
			t.Error("two days share a key")
		}
		// A non-UTC clock must not shift the date.
		ist := time.FixedZone("IST", 5*3600+1800)
		local := NewOverdueEvent(task, time.Date(2026, 11, 14, 2, 0, 0, 0, ist)) // 20:30 UTC on the 13th
		if got := local.IdempotencyKey("usr_bob"); got != want {
			t.Errorf("key from a local time = %q, want %q", got, want)
		}
	})

	t.Run("two comments on one task have different keys", func(t *testing.T) {
		_, e1, _ := NewComment(task, "first", alice, t0)
		_, e2, _ := NewComment(task, "second", alice, t0)
		if e1[0].IdempotencyKey("usr_bob") == e2[0].IdempotencyKey("usr_bob") {
			t.Error("two comments share a key")
		}
	})

	t.Run("keys are deterministic", func(t *testing.T) {
		e := task.event(EventTaskBlocked, alice, t0)
		first := e.IdempotencyKey("usr_bob")
		for i := 0; i < 5; i++ {
			if got := task.event(EventTaskBlocked, alice, t0).IdempotencyKey("usr_bob"); got != first {
				t.Fatalf("same input produced %q, then %q", first, got)
			}
		}
		again := task.event(EventTaskBlocked, alice, t0.Add(time.Hour))
		if e.IdempotencyKey("usr_bob") != again.IdempotencyKey("usr_bob") {
			t.Error("a mutation key depends on wall-clock time")
		}
	})
}

// TestBlockedNotificationMatchesContract compares a p1_critical blocked
// notification against the example in docs/archive/initial.md section 4.C.2.
func TestBlockedNotificationMatchesContract(t *testing.T) {
	due := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	task, _, err := NewTask(NewTaskInput{
		TenantID:    "tenant_acme",
		WorkspaceID: "ws_core_eng",
		Title:       "Production DB migration script failing on staging",
		Priority:    PriorityCritical,
		AssigneeID:  "usr_assignee_42",
		Labels:      []string{"database", "release-blocker"},
		DueAt:       &due,
	}, alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.TransitionTo(StatusInProgress, alice, t0); err != nil {
		t.Fatal(err)
	}
	events, err := task.TransitionTo(StatusBlocked, User("usr_dev_19"), t0)
	if err != nil {
		t.Fatal(err)
	}
	ns := events[0].Notifications(baseURL)
	if len(ns) != 1 {
		t.Fatalf("notifications = %d, want 1", len(ns))
	}
	n := ns[0]
	id := task.ID.String()

	if want := "taskify:tenant_acme:" + id + ":task.blocked:v3"; n.IdempotencyKey != want {
		t.Errorf("idempotencyKey = %q, want %q", n.IdempotencyKey, want)
	}
	if n.TenantID != "tenant_acme" || n.UserID != "usr_assignee_42" || n.Source != "taskify" || n.EventType != "task.blocked" {
		t.Errorf("envelope = %+v", n)
	}
	if n.Title != "[BLOCKED] Production DB migration script failing on staging" {
		t.Errorf("title = %q", n.Title)
	}
	if want := "Task #" + id[:8] + " was marked as BLOCKED by usr_dev_19. Priority: P1 Critical."; n.Body != want {
		t.Errorf("body = %q, want %q", n.Body, want)
	}
	if n.PriorityHint != "critical" {
		t.Errorf("priorityHint = %q", n.PriorityHint)
	}
	if !slices.Equal(n.Channels, []string{"slack", "email", "in_app"}) {
		t.Errorf("channels = %v", n.Channels)
	}

	// Check the wire format: camelCase keys and the expected payload.
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"idempotencyKey", "tenantId", "userId", "source", "eventType", "title", "body", "priorityHint", "channels", "payload"} {
		if _, ok := wire[k]; !ok {
			t.Errorf("wire format is missing %q", k)
		}
	}
	p := wire["payload"].(map[string]any)
	want := map[string]any{
		"taskId":         id,
		"workspaceId":    "ws_core_eng",
		"status":         "blocked",
		"previousStatus": "in_progress",
		"priority":       "p1_critical",
		"dueAt":          "2026-11-12T14:00:00Z",
		"actorId":        "usr_dev_19",
		"version":        float64(3),
		"bypassDigest":   true,
		"blockedReason":  "manual",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("payload.%s = %v, want %v", k, p[k], v)
		}
	}
	if labels := p["labels"].([]any); len(labels) != 2 || labels[0] != "database" {
		t.Errorf("payload.labels = %v", labels)
	}
	urls := p["actionUrls"].(map[string]any)
	if want := baseURL + "/api/v1/tasks/" + id + "/feedback?signal=acted"; urls["acknowledge"] != want {
		t.Errorf("actionUrls.acknowledge = %v", urls["acknowledge"])
	}
	if want := baseURL + "/api/v1/tasks/" + id + "/feedback?signal=snoozed&duration=2h"; urls["snooze_2h"] != want {
		t.Errorf("actionUrls.snooze_2h = %v", urls["snooze_2h"])
	}
	for k := range p {
		if strings.Contains(k, "_") {
			t.Errorf("payload key %q is not camelCase", k)
		}
	}
}

func TestNotificationRouting(t *testing.T) {
	notify := func(t *testing.T, priority Priority, patch Patch) Notification {
		t.Helper()
		task, _, err := NewTask(NewTaskInput{TenantID: "t", WorkspaceID: "w", Title: "x", Priority: priority, AssigneeID: bob.ID}, alice, t0)
		if err != nil {
			t.Fatal(err)
		}
		events, err := task.Apply(patch, alice, t0)
		if err != nil || len(events) == 0 {
			t.Fatalf("Apply: events %v err %v", events, err)
		}
		return events[0].Notifications(baseURL)[0]
	}

	t.Run("an ordinary update leaves routing to AgentPulse", func(t *testing.T) {
		n := notify(t, PriorityMedium, Patch{Title: ptr("y")})
		if n.Channels != nil || n.Payload["bypassDigest"] != nil || n.PriorityHint != "medium" {
			t.Errorf("channels %v bypass %v hint %s", n.Channels, n.Payload["bypassDigest"], n.PriorityHint)
		}
		raw, _ := json.Marshal(n)
		if strings.Contains(string(raw), `"channels"`) {
			t.Error("empty channels must be omitted from the wire format")
		}
		if n.Payload["labels"] == nil {
			t.Error("payload.labels must be an empty list, not null")
		}
	})

	t.Run("a critical task bypasses the digest for any event", func(t *testing.T) {
		n := notify(t, PriorityCritical, Patch{Title: ptr("y")})
		if n.Payload["bypassDigest"] != true || n.PriorityHint != "critical" {
			t.Errorf("bypass %v hint %s", n.Payload["bypassDigest"], n.PriorityHint)
		}
	})

	t.Run("a manual block is at least high and bypasses the digest", func(t *testing.T) {
		n := notify(t, PriorityLow, Patch{Status: ptr(StatusBlocked)})
		if n.Payload["bypassDigest"] != true || n.PriorityHint != "high" {
			t.Errorf("bypass %v hint %s", n.Payload["bypassDigest"], n.PriorityHint)
		}
	})

	t.Run("a dependency block is quiet", func(t *testing.T) {
		n := notify(t, PriorityLow, Patch{Status: ptr(StatusBlocked), BlockedReason: BlockedDependency})
		if n.Payload["bypassDigest"] != nil || n.Channels != nil || n.PriorityHint != "low" {
			t.Errorf("bypass %v channels %v hint %s", n.Payload["bypassDigest"], n.Channels, n.PriorityHint)
		}
		if n.Payload["blockedReason"] != "dependency" {
			t.Errorf("blockedReason = %v", n.Payload["blockedReason"])
		}
	})
}

func TestNotificationTextForEveryEvent(t *testing.T) {
	due := t0.Add(time.Hour)
	task := newTask(t)
	task.DueAt = &due
	comment, commentEvents, err := NewComment(task, "please look", alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = comment

	events := []Event{
		task.event(EventTaskCreated, alice, t0),
		task.event(EventTaskAssigned, alice, t0),
		task.event(EventTaskUpdated, alice, t0),
		task.event(EventTaskPriorityEscalated, alice, t0),
		task.event(EventTaskBlocked, alice, t0),
		NewDueSoonEvent(task, DueWindow24h, t0),
		NewDueSoonEvent(task, DueWindow1h, t0),
		NewOverdueEvent(task, t0),
		task.event(EventTaskCompleted, bob, t0),
		commentEvents[0],
	}
	seen := map[string]bool{}
	for _, e := range events {
		ns := e.Notifications(baseURL)
		if len(ns) == 0 {
			t.Errorf("%s: no notifications", e)
			continue
		}
		n := ns[0]
		if !strings.Contains(n.Title, task.Title) || n.Body == "" {
			t.Errorf("%s: title %q body %q", e.Type, n.Title, n.Body)
		}
		if seen[n.Title] {
			t.Errorf("%s: title %q is not distinct", e.Type, n.Title)
		}
		seen[n.Title] = true
	}

	if e := NewDueSoonEvent(task, DueWindow1h, t0); e.Notifications(baseURL)[0].Payload["dueWindow"] != "1h" {
		t.Error("due-soon payload is missing dueWindow")
	}
	if e := NewDueSoonEvent(task, DueWindow24h, t0); e.Actor.Kind != ActorSystem {
		t.Error("scheduler events must carry the system actor")
	}
	if got := task.event(EventTaskBlocked, alice, t0).String(); got != "task.blocked v1" {
		t.Errorf("String() = %q", got)
	}

	// A task without a due date still renders.
	task.DueAt = nil
	if body := NewOverdueEvent(task, t0).Notifications(baseURL)[0].Body; !strings.Contains(body, "unknown time") {
		t.Errorf("body = %q", body)
	}
}

func TestExtractMentions(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"no mention here", []string{}},
		{"@bob can you look?", []string{"bob"}},
		{"ping @bob and @carol.", []string{"bob", "carol"}},
		{"@bob @bob again @bob", []string{"bob"}},
		{"(@dave) and [@erin], then @frank!", []string{"dave", "erin", "frank"}},
		{"mail dev@example.com about it", []string{}},
		{"cc @first.last and @under_score-x", []string{"first.last", "under_score-x"}},
		{"line one\n@nina on line two", []string{"nina"}},
		{"a lone @ sign and @@double", []string{}},
		{"thanks @usr_bob-", []string{"usr_bob"}},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			if got := ExtractMentions(tc.body); !slices.Equal(got, tc.want) {
				t.Errorf("ExtractMentions = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewComment(t *testing.T) {
	task := newTask(t) // created by alice, assigned to bob, watched by carol

	t.Run("mentioned users are recipients and flagged", func(t *testing.T) {
		c, events, err := NewComment(task, "  @usr_dave please review, cc @usr_bob  ", alice, t0)
		if err != nil {
			t.Fatal(err)
		}
		if c.Body != "@usr_dave please review, cc @usr_bob" || c.TaskID != task.ID || c.AuthorID != alice.ID {
			t.Errorf("comment = %+v", c)
		}
		if !slices.Equal(c.Mentions, []string{"usr_dave", "usr_bob"}) {
			t.Errorf("Mentions = %v", c.Mentions)
		}
		if task.Version != 1 {
			t.Errorf("a comment changed the task version to %d", task.Version)
		}
		if len(events) != 1 || events[0].Type != EventTaskCommentAdded {
			t.Fatalf("events = %v", events)
		}

		flags := map[string]any{}
		for _, n := range events[0].Notifications(baseURL) {
			flags[n.UserID] = n.Payload["isMentioned"]
			if n.Payload["commentId"] != c.ID.String() {
				t.Errorf("commentId = %v", n.Payload["commentId"])
			}
			if wantSuffix := ":" + c.ID.String() + ":" + n.UserID; !strings.HasSuffix(n.IdempotencyKey, wantSuffix) {
				t.Errorf("key %q does not end with %q", n.IdempotencyKey, wantSuffix)
			}
		}
		want := map[string]any{"usr_bob": true, "usr_carol": false, "usr_dave": true}
		if len(flags) != len(want) {
			t.Fatalf("recipients = %v, want %v", flags, want)
		}
		for u, f := range want {
			if flags[u] != f {
				t.Errorf("isMentioned for %s = %v, want %v", u, flags[u], f)
			}
		}
	})

	t.Run("the author is not notified, even when self-mentioned", func(t *testing.T) {
		_, events, err := NewComment(task, "note to self @usr_bob", bob, t0)
		if err != nil {
			t.Fatal(err)
		}
		if got := events[0].Recipients(); slices.Contains(got, "usr_bob") {
			t.Errorf("Recipients = %v, want the author excluded", got)
		}
	})

	t.Run("a long comment is truncated in the notification", func(t *testing.T) {
		_, events, err := NewComment(task, strings.Repeat("é", 500), alice, t0)
		if err != nil {
			t.Fatal(err)
		}
		body := events[0].Notifications(baseURL)[0].Body
		if !strings.HasSuffix(body, "...") || len([]rune(body)) > 320 {
			t.Errorf("body length = %d runes", len([]rune(body)))
		}
	})

	t.Run("validation", func(t *testing.T) {
		for name, tc := range map[string]struct {
			body  string
			actor Actor
		}{
			"empty body":     {"   ", alice},
			"oversized body": {strings.Repeat("x", maxCommentLen+1), alice},
			"missing author": {"hello", User(" ")},
		} {
			if _, _, err := NewComment(task, tc.body, tc.actor, t0); !errors.Is(err, ErrValidation) {
				t.Errorf("%s: error = %v, want ErrValidation", name, err)
			}
		}
	})
}

func TestTransitionErrorMessage(t *testing.T) {
	task := taskIn(t, StatusArchived)
	_, err := task.TransitionTo(StatusCompleted, alice, t0)
	if err == nil || err.Error() != "invalid state transition: archived to completed" {
		t.Errorf("error = %v", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Error("a transition error must not match ErrValidation")
	}
}
