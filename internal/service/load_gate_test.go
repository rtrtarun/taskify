package service

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// notification returns the one outbox notification of eventType for the
// task, or fails the test.
func (f *fixture) notification(t *testing.T, taskID uuid.UUID, eventType string) domain.Notification {
	t.Helper()
	entries, err := f.b.Outbox.ListForAggregate(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var found []domain.Notification
	for _, e := range entries {
		if e.EventType != eventType {
			continue
		}
		var n domain.Notification
		if err := json.Unmarshal(e.Payload, &n); err != nil {
			t.Fatal(err)
		}
		found = append(found, n)
	}
	if len(found) != 1 {
		t.Fatalf("%s notifications for the task = %d, want 1", eventType, len(found))
	}
	return found[0]
}

func deferred(n domain.Notification) bool {
	return slices.Equal(n.Channels, []string{domain.ChannelDigest}) && n.Payload["digestSummary"] == domain.DeferredDigestSummary &&
		n.Payload["bypassDigest"] == nil
}

func immediate(n domain.Notification) bool {
	return n.Payload["bypassDigest"] == true && slices.Equal(n.Channels, []string{"slack", "email", "in_app"}) &&
		n.Payload["digestSummary"] == nil
}

// routedByAgentPulse is a notification Taskify neither deferred nor forced.
func routedByAgentPulse(n domain.Notification) bool {
	return n.Channels == nil && n.Payload["digestSummary"] == nil && n.Payload["bypassDigest"] == nil
}

// critical gives bob an in_progress p1_critical task in the workspace.
func (f *fixture) critical(t *testing.T, workspace string) *domain.Task {
	t.Helper()
	task := f.create(t, func(in *domain.NewTaskInput) {
		in.WorkspaceID, in.Priority, in.Title, in.Watchers = workspace, domain.PriorityCritical, "Outage", nil
	})
	return f.edit(t, bob, task, domain.Patch{Status: ptr(domain.StatusInProgress)})
}

// assign gives bob a task created unassigned by alice, and returns the
// task.assigned notification.
func (f *fixture) assign(t *testing.T, actor domain.Actor, priority domain.Priority, to string) domain.Notification {
	t.Helper()
	task := f.create(t, func(in *domain.NewTaskInput) { in.AssigneeID, in.Priority, in.Watchers = "", priority, nil })
	task = f.edit(t, actor, task, domain.Patch{AssigneeID: ptr(to)})
	return f.notification(t, task.ID, domain.EventTaskAssigned)
}

// TestLoadGatekeeper covers M10-01 and the first two M10 exit criteria: a
// non-critical assignment to a user at or over the threshold is delivered
// on the digest channel only; a critical one is still immediate; with the
// digest agent disabled nothing is deferred.
func TestLoadGatekeeper(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		other, err := f.svc.CreateWorkspace(ctx, tenant, "ws_other", "Other") // agents off there
		if err != nil {
			t.Fatal(err)
		}
		// Load that does not count: another person's, another tenant's, a
		// critical task not yet started, a started task that is not
		// critical, and a critical task that is blocked.
		if _, err := f.svc.CreateWorkspace(ctx, "tenant_other", "ws_core", "Core"); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			elsewhere, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_other", WorkspaceID: "ws_core",
				Title: "Theirs", Priority: domain.PriorityCritical, AssigneeID: bob.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.UpdateTask(ctx, bob, "tenant_other", elsewhere.ID, elsewhere.Version,
				domain.Patch{Status: ptr(domain.StatusInProgress)}); err != nil {
				t.Fatal(err)
			}
		}
		carols := f.create(t, func(in *domain.NewTaskInput) {
			in.Priority, in.AssigneeID, in.Watchers = domain.PriorityCritical, "usr_carol", nil
		})
		f.edit(t, domain.User("usr_carol"), carols, domain.Patch{Status: ptr(domain.StatusInProgress)})
		f.create(t, func(in *domain.NewTaskInput) { in.Priority, in.Watchers = domain.PriorityCritical, nil })
		high := f.create(t, func(in *domain.NewTaskInput) { in.Priority, in.Watchers = domain.PriorityHigh, nil })
		f.edit(t, bob, high, domain.Patch{Status: ptr(domain.StatusInProgress)})
		f.edit(t, alice, f.critical(t, "ws_core"), domain.Patch{Status: ptr(domain.StatusBlocked)})

		// Two in progress, one of them in a workspace with agents off: the
		// load is the person's, across the tenant. Below the threshold.
		f.critical(t, "ws_core")
		f.critical(t, other.ID)
		if n := f.assign(t, alice, domain.PriorityMedium, bob.ID); !routedByAgentPulse(n) {
			t.Errorf("below the threshold: %+v", n)
		}

		// A third: at the threshold, which counts as overloaded.
		f.critical(t, "ws_core")
		if n := f.assign(t, alice, domain.PriorityHigh, bob.ID); !deferred(n) {
			t.Errorf("a non-critical assignment to an overloaded user: channels %v payload %v", n.Channels, n.Payload)
		}
		if n := f.assign(t, alice, domain.PriorityCritical, bob.ID); !immediate(n) {
			t.Errorf("a critical assignment to an overloaded user: channels %v payload %v", n.Channels, n.Payload)
		}
		// Creating a task assigned to bob is his assignment notice (D1).
		created := f.create(t, func(in *domain.NewTaskInput) { in.Priority, in.Watchers = domain.PriorityLow, nil })
		if n := f.notification(t, created.ID, domain.EventTaskCreated); !deferred(n) {
			t.Errorf("a new task for an overloaded user: channels %v payload %v", n.Channels, n.Payload)
		}
		// An agent's reassignment is an assignment too, and queues no job.
		if n := f.assign(t, domain.Agent(domain.AgentBlocker), domain.PriorityMedium, bob.ID); !deferred(n) {
			t.Errorf("an agent's assignment: channels %v payload %v", n.Channels, n.Payload)
		}
		// Only assignments are held: an edit to bob's task is not.
		edited := f.edit(t, alice, created, domain.Patch{Title: ptr("Renamed")})
		if n := f.notification(t, edited.ID, domain.EventTaskUpdated); !routedByAgentPulse(n) {
			t.Errorf("an update for an overloaded user: %+v", n)
		}
		// Somebody else is not overloaded.
		if n := f.assign(t, alice, domain.PriorityMedium, "usr_carol"); !routedByAgentPulse(n) {
			t.Errorf("an assignment to carol: %+v", n)
		}
		// A task created in a workspace with agents off is not held.
		elsewhere := f.create(t, func(in *domain.NewTaskInput) { in.WorkspaceID, in.Watchers = other.ID, nil })
		if n := f.notification(t, elsewhere.ID, domain.EventTaskCreated); !routedByAgentPulse(n) {
			t.Errorf("a workspace with agents off: %+v", n)
		}

		// The digest agent disabled for the workspace: nothing is deferred.
		f.setAgents(t, domain.AgentSettingsPatch{DisabledAgents: &[]string{domain.AgentDigest}})
		if n := f.assign(t, alice, domain.PriorityMedium, bob.ID); !routedByAgentPulse(n) {
			t.Errorf("digest agent disabled: %+v", n)
		}
		// Agents switched off for the workspace: nothing is deferred.
		f.setAgents(t, domain.AgentSettingsPatch{AgentsEnabled: ptr(false), DisabledAgents: &[]string{}})
		if n := f.assign(t, alice, domain.PriorityMedium, bob.ID); !routedByAgentPulse(n) {
			t.Errorf("agents off: %+v", n)
		}
	})
}

// TestLoadGatekeeperThreshold: the threshold comes from the configuration
// (DIGEST_LOAD_THRESHOLD).
func TestLoadGatekeeperThreshold(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		f.svc = NewTaskService(f.svc.deps, Config{PublicBaseURL: "https://taskify.test", OutboxMaxAttempts: 7, DigestLoadThreshold: 1},
			WithClock(func() time.Time { return f.now }))
		if n := f.assign(t, alice, domain.PriorityLow, bob.ID); !routedByAgentPulse(n) {
			t.Errorf("no load: %+v", n)
		}
		f.critical(t, "ws_core")
		if n := f.assign(t, alice, domain.PriorityLow, bob.ID); !deferred(n) {
			t.Errorf("one critical task with a threshold of 1: %+v", n)
		}
		if got := NewTaskService(f.svc.deps, Config{}).cfg.DigestLoadThreshold; got != 3 {
			t.Errorf("default threshold = %d, want 3", got)
		}
	})
}
