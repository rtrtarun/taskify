package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func newTaskT(t *testing.T, title, assignee string, p Priority) *Task {
	t.Helper()
	task, _, err := NewTask(NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: title, AssigneeID: assignee, Priority: p}, alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestNewDependency(t *testing.T) {
	a, b := newTaskT(t, "A", "", ""), newTaskT(t, "B", "", "")
	d, err := NewDependency(a, b, t0)
	if err != nil || d.TaskID != a.ID || d.DependsOnTaskID != b.ID || d.TenantID != "tenant_acme" {
		t.Errorf("dependency = %+v, %v", d, err)
	}
	if _, err := NewDependency(a, a, t0); !errors.Is(err, ErrValidation) {
		t.Errorf("self: %v", err)
	}
	other := *b
	other.TenantID = "tenant_other"
	if _, err := NewDependency(a, &other, t0); !errors.Is(err, ErrValidation) {
		t.Errorf("cross-tenant: %v", err)
	}
}

func TestDependencyBlockAndLift(t *testing.T) {
	blocker := newTaskT(t, "Blocker", "", "")
	dep := newTaskT(t, "Dependent", "usr_bob", PriorityCritical)

	events, err := dep.ApplyDependencyBlock(blocker, t0)
	if err != nil || dep.Status != StatusBlocked || *dep.BlockedReason != BlockedDependency || len(events) != 1 {
		t.Fatalf("block: %s %v %v", dep.Status, events, err)
	}
	if events[0].Actor.Kind != ActorSystem || events[0].Blocker.ID != blocker.ID {
		t.Errorf("block event = %+v", events[0])
	}
	// A critical task still bypasses the digest; that is about its priority,
	// not the block.
	n := events[0].Notifications("https://t")[0]
	if n.Payload["blockedReason"] != "dependency" || n.PriorityHint != "critical" {
		t.Errorf("notification = %+v", n)
	}

	// Blocking again, or blocking a task that is not todo, does nothing.
	if ev, _ := dep.ApplyDependencyBlock(blocker, t0); ev != nil {
		t.Error("a blocked task was blocked again")
	}

	events, err = dep.LiftDependencyBlock(blocker, t0)
	if err != nil || dep.Status != StatusTodo || dep.BlockedReason != nil || len(events) != 1 {
		t.Fatalf("lift: %s %v %v", dep.Status, events, err)
	}
	if e := events[0]; e.Type != EventTaskUnblocked || e.PreviousStatus != StatusBlocked {
		t.Errorf("lift event = %+v", e)
	}

	// A manual block is never lifted by the system.
	to := StatusBlocked
	if _, err := dep.Apply(Patch{Status: &to}, alice, t0); err != nil {
		t.Fatal(err)
	}
	if ev, _ := dep.LiftDependencyBlock(blocker, t0); ev != nil || dep.Status != StatusBlocked {
		t.Error("a manual block was lifted")
	}
}

func TestDependencyNotices(t *testing.T) {
	blocker := newTaskT(t, "Vendor key", "", "")
	dep := newTaskT(t, "Integrate", "usr_bob", PriorityLow)
	base := "taskify:tenant_acme:" + dep.ID.String() + ":"

	for _, tc := range []struct {
		typ, hint, title string
	}{
		{EventTaskDependencyAdded, "low", "New dependency: "},
		{EventTaskDependencyResolved, "low", "Dependency done: "},
		{EventTaskDependencyReopened, "high", "Dependency reopened: "},
	} {
		e := NewDependencyEvent(tc.typ, dep, blocker, t0)
		ns := e.Notifications("https://t")
		if len(ns) != 1 || ns[0].UserID != "usr_bob" {
			t.Fatalf("%s: notifications = %+v", tc.typ, ns)
		}
		n := ns[0]
		if want := base + tc.typ + ":" + blocker.ID.String() + ":v1"; n.IdempotencyKey != want {
			t.Errorf("%s: key = %q, want %q", tc.typ, n.IdempotencyKey, want)
		}
		if n.PriorityHint != tc.hint || !strings.HasPrefix(n.Title, tc.title) || n.Payload["bypassDigest"] != nil {
			t.Errorf("%s: %+v", tc.typ, n)
		}
		if !strings.Contains(n.Body, "Vendor key") {
			t.Errorf("%s: body %q does not name the blocker", tc.typ, n.Body)
		}
	}

	// The same notice about two different blockers must not share a key.
	other := newTaskT(t, "Other", "", "")
	k1 := NewDependencyEvent(EventTaskDependencyAdded, dep, blocker, t0).IdempotencyKey("usr_bob")
	k2 := NewDependencyEvent(EventTaskDependencyAdded, dep, other, t0).IdempotencyKey("usr_bob")
	if k1 == k2 {
		t.Error("notices about different blockers share a key")
	}

	// An unassigned dependent notifies nobody.
	if ns := NewDependencyEvent(EventTaskDependencyResolved, blocker, dep, t0).Notifications("https://t"); len(ns) != 0 {
		t.Errorf("unassigned: %+v", ns)
	}
}

func TestSystemComment(t *testing.T) {
	task := newTaskT(t, "T", "", "")
	c, err := NewSystemComment(task, "Dependency done.", t0)
	if err != nil || c.AuthorID != System.ID || c.TaskID != task.ID || c.ID == uuid.Nil {
		t.Errorf("comment = %+v, %v", c, err)
	}
	if !(&Task{Status: StatusArchived}).ResolvesDependencies() || (&Task{Status: StatusBlocked}).ResolvesDependencies() {
		t.Error("ResolvesDependencies")
	}
}
