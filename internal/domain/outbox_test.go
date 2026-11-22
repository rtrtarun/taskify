package domain

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNewOutboxEntries(t *testing.T) {
	task := newTask(t) // assigned to bob, watched by carol
	events, err := task.Apply(Patch{Title: ptr("Renamed")}, alice, t0)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := NewOutboxEntries(events, baseURL, 10, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want one per recipient (2)", len(entries))
	}
	var users []string
	for _, e := range entries {
		if e.Status != OutboxPending || e.MaxAttempts != 10 || e.Attempts != 0 {
			t.Errorf("entry state = %s attempts %d/%d", e.Status, e.Attempts, e.MaxAttempts)
		}
		if !e.NextAttemptAt.Equal(t0) || !e.CreatedAt.Equal(t0) || e.ClaimedAt != nil {
			t.Errorf("entry times = %v %v %v", e.NextAttemptAt, e.CreatedAt, e.ClaimedAt)
		}
		if e.AggregateID != task.ID || e.TenantID != "tenant_acme" || e.EventType != EventTaskUpdated {
			t.Errorf("entry identity = %+v", e)
		}
		if e.ID.Version() != 7 || e.TraceContext == nil {
			t.Errorf("ID %v TraceContext %v", e.ID, e.TraceContext)
		}
		var n Notification
		if err := json.Unmarshal(e.Payload, &n); err != nil {
			t.Fatal(err)
		}
		if n.IdempotencyKey != e.IdempotencyKey {
			t.Errorf("payload key %q differs from column %q", n.IdempotencyKey, e.IdempotencyKey)
		}
		users = append(users, n.UserID)
	}
	if !slices.Equal(users, []string{"usr_bob", "usr_carol"}) {
		t.Errorf("recipients = %v", users)
	}

	none, err := NewOutboxEntries(nil, baseURL, 10, t0)
	if err != nil || len(none) != 0 {
		t.Errorf("no events: entries %v err %v", none, err)
	}
}

func TestNewWorkspace(t *testing.T) {
	w, err := NewWorkspace("tenant_acme", " ws_core-1 ", "  Core  ", t0)
	if err != nil {
		t.Fatal(err)
	}
	if w.TenantID != "tenant_acme" || w.ID != "ws_core-1" || w.Name != "Core" || !w.CreatedAt.Equal(t0) {
		t.Errorf("workspace = %+v", w)
	}

	cases := map[string][3]string{
		"missing tenant":  {"", "ws", "n"},
		"long tenant":     {strings.Repeat("t", 65), "ws", "n"},
		"padded tenant":   {" tenant_acme ", "ws", "n"},
		"reserved tenant": {"default", "ws", "n"},
		"empty id":        {"t", "", "n"},
		"id with space":   {"t", "ws core", "n"},
		"long id":         {"t", strings.Repeat("w", 65), "n"},
		"missing name":    {"t", "ws", " "},
		"long name":       {"t", "ws", strings.Repeat("n", 256)},
	}
	for name, in := range cases {
		if _, err := NewWorkspace(in[0], in[1], in[2], t0); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: error = %v, want ErrValidation", name, err)
		}
	}
}

func TestWatchersAreASortedSet(t *testing.T) {
	task, _, err := NewTask(NewTaskInput{TenantID: "t", WorkspaceID: "w", Title: "x", Watchers: []string{"zed", "amy", "zed", " "}}, alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(task.Watchers, []string{"amy", "zed"}) {
		t.Errorf("Watchers = %v", task.Watchers)
	}
	// The same set in another order is not a change.
	events, err := task.Apply(Patch{Watchers: ptr([]string{"zed", "amy"})}, alice, t0)
	if err != nil || len(events) != 0 || task.Version != 1 {
		t.Errorf("reordered watchers counted as a change: events %v version %d err %v", events, task.Version, err)
	}
}

func TestWorkspaceRename(t *testing.T) {
	w, _ := NewWorkspace("t", "ws", "Old", t0)
	if err := w.Rename("  New  ", t0.Add(time.Hour)); err != nil || w.Name != "New" || !w.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("Rename = %+v, %v", w, err)
	}
	for _, bad := range []string{" ", strings.Repeat("n", 256)} {
		if err := w.Rename(bad, t0); !errors.Is(err, ErrValidation) || w.Name != "New" {
			t.Errorf("Rename(%d chars): error = %v, name %q", len(bad), err, w.Name)
		}
	}
}
