package service

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// notifications returns the /notify bodies written for a task, oldest first.
func (f *fixture) notifications(t *testing.T, taskID uuid.UUID) []domain.Notification {
	t.Helper()
	entries, err := f.b.Outbox.ListForAggregate(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := []domain.Notification{}
	for _, e := range entries {
		var n domain.Notification
		if err := json.Unmarshal(e.Payload, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func (f *fixture) comments(t *testing.T, taskID uuid.UUID) []domain.Comment {
	t.Helper()
	c, err := f.b.Comments.ListRecent(ctx, taskID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fixture) get(t *testing.T, id uuid.UUID) *domain.Task {
	t.Helper()
	task, err := f.b.Tasks.Get(ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *fixture) link(t *testing.T, dependent, blocker *domain.Task) *domain.Task {
	t.Helper()
	task, err := f.svc.AddDependency(ctx, tenant, dependent.ID, blocker.ID)
	if err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	return task
}

func (f *fixture) setStatus(t *testing.T, task *domain.Task, s domain.Status) *domain.Task {
	t.Helper()
	cur := f.get(t, task.ID)
	out, err := f.svc.UpdateTask(ctx, alice, tenant, task.ID, cur.Version, domain.Patch{Status: &s})
	if err != nil {
		t.Fatalf("set %s: %v", s, err)
	}
	return out
}

// unassigned creates a task that notifies nobody, so only the events under
// test reach the outbox.
func (f *fixture) unassigned(t *testing.T, title string) *domain.Task {
	return f.create(t, func(in *domain.NewTaskInput) { in.Title = title; in.AssigneeID = ""; in.Watchers = nil })
}

func TestAddDependencyBlocksTodoQuietly(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		blocker := f.unassigned(t, "Migrate schema")
		dep := f.create(t, func(in *domain.NewTaskInput) { in.Priority = domain.PriorityHigh; in.Watchers = nil })
		before := len(f.notifications(t, dep.ID))

		got := f.link(t, dep, blocker)
		if got.Status != domain.StatusBlocked || *got.BlockedReason != domain.BlockedDependency ||
			*got.StatusBeforeBlocked != domain.StatusTodo || got.Version != dep.Version+1 {
			t.Fatalf("dependent = %+v", got)
		}
		ns := f.notifications(t, dep.ID)[before:]
		if len(ns) != 1 || ns[0].EventType != domain.EventTaskBlocked {
			t.Fatalf("notifications = %+v", ns)
		}
		n := ns[0]
		if n.Payload["blockedReason"] != "dependency" || n.Payload["actorKind"] != "system" ||
			n.Payload["bypassDigest"] != nil || n.Channels != nil || n.PriorityHint != "high" ||
			n.Payload["blockerTaskId"] != blocker.ID.String() {
			t.Errorf("a dependency block is not quiet: %+v", n)
		}

		blockers, _ := f.b.Deps.ListBlockers(ctx, tenant, dep.ID)
		if !slices.Equal(blockers, []uuid.UUID{blocker.ID}) {
			t.Errorf("blockers = %v", blockers)
		}

		// Adding the same link again changes nothing.
		again := f.link(t, dep, blocker)
		if again.Version != got.Version || len(f.notifications(t, dep.ID)) != before+1 {
			t.Errorf("a repeated link changed the task: v%d", again.Version)
		}
	})
}

func TestAddDependencyLeavesInProgressAlone(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		blocker := f.unassigned(t, "Vendor API key")
		dep := f.create(t, func(in *domain.NewTaskInput) { in.Watchers = nil })
		dep = f.setStatus(t, dep, domain.StatusInProgress)
		before := len(f.notifications(t, dep.ID))

		got := f.link(t, dep, blocker)
		if got.Status != domain.StatusInProgress || got.Version != dep.Version {
			t.Errorf("an in_progress dependent changed: %s v%d", got.Status, got.Version)
		}
		ns := f.notifications(t, dep.ID)[before:]
		if len(ns) != 1 || ns[0].EventType != domain.EventTaskDependencyAdded || ns[0].UserID != bob.ID {
			t.Fatalf("notifications = %+v", ns)
		}
		cs := f.comments(t, dep.ID)
		if len(cs) != 1 || cs[0].AuthorID != domain.System.ID {
			t.Fatalf("comments = %+v", cs)
		}
	})
}

func TestAddDependencyOnResolvedBlocker(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		done := f.unassigned(t, "Done already")
		f.setStatus(t, done, domain.StatusCompleted)
		dep := f.unassigned(t, "Next step")

		got := f.link(t, dep, done)
		if got.Status != domain.StatusTodo || got.Version != dep.Version {
			t.Errorf("a resolved blocker changed the dependent: %s v%d", got.Status, got.Version)
		}
		if open, _ := f.b.Deps.HasOpenBlocker(ctx, tenant, dep.ID); open {
			t.Error("a completed blocker counts as open")
		}
	})
}

func TestAddDependencyRejectsCycles(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		a, b, c := f.unassigned(t, "A"), f.unassigned(t, "B"), f.unassigned(t, "C")
		if _, err := f.svc.AddDependency(ctx, tenant, a.ID, a.ID); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("self-dependency: error = %v", err)
		}
		f.link(t, a, b) // A waits on B
		f.link(t, b, c) // B waits on C
		for name, pair := range map[string][2]uuid.UUID{
			"direct":     {b.ID, a.ID},
			"transitive": {c.ID, a.ID},
		} {
			if _, err := f.svc.AddDependency(ctx, tenant, pair[0], pair[1]); !errors.Is(err, ErrDependencyCycle) {
				t.Errorf("%s cycle: error = %v, want ErrDependencyCycle", name, err)
			}
		}
		// A rejected link leaves the task untouched.
		if got := f.get(t, c.ID); got.Status != domain.StatusTodo {
			t.Errorf("a rejected link changed C: %s", got.Status)
		}
	})
}

func TestAddDependencyAcrossTenants(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		if _, err := f.svc.CreateWorkspace(ctx, "tenant_other", "ws_core", "Other"); err != nil {
			t.Fatal(err)
		}
		mine := f.unassigned(t, "Mine")
		theirs, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_other", WorkspaceID: "ws_core", Title: "Theirs"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.AddDependency(ctx, tenant, mine.ID, theirs.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("link to another tenant's task: error = %v, want ErrNotFound", err)
		}
		if _, err := f.svc.AddDependency(ctx, "tenant_other", theirs.ID, mine.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("link from another tenant's task: error = %v, want ErrNotFound", err)
		}
		if blockers, _ := f.b.Deps.ListBlockers(ctx, tenant, mine.ID); len(blockers) != 0 {
			t.Errorf("a cross-tenant link was stored: %v", blockers)
		}
	})
}

// eventTypes returns the event types written for a task after the first skip.
func (f *fixture) eventTypes(t *testing.T, id uuid.UUID, skip int) []string {
	t.Helper()
	var out []string
	for _, n := range f.notifications(t, id)[skip:] {
		out = append(out, n.EventType)
	}
	return out
}

func TestArchivingOnlyBlockerUnblocks(t *testing.T) {
	for _, terminal := range []domain.Status{domain.StatusArchived, domain.StatusCompleted} {
		t.Run(string(terminal), func(t *testing.T) {
			each(t, func(t *testing.T, f *fixture) {
				blocker := f.unassigned(t, "Blocker")
				dep := f.create(t, func(in *domain.NewTaskInput) { in.Watchers = nil })
				f.link(t, dep, blocker)
				skip := len(f.notifications(t, dep.ID))

				f.setStatus(t, blocker, terminal)
				got := f.get(t, dep.ID)
				if got.Status != domain.StatusTodo || got.BlockedReason != nil || got.StatusBeforeBlocked != nil {
					t.Errorf("dependent after %s: %s", terminal, got.Status)
				}
				ns := f.notifications(t, dep.ID)[skip:]
				if len(ns) != 1 || ns[0].EventType != domain.EventTaskUnblocked {
					t.Fatalf("events = %v, want exactly one task.unblocked", f.eventTypes(t, dep.ID, skip))
				}
				if ns[0].Payload["blockerStatus"] != string(terminal) || ns[0].Payload["actorKind"] != "system" {
					t.Errorf("unblocked payload = %v", ns[0].Payload)
				}
			})
		})
	}
}

func TestTwoBlockersNeedBoth(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		b1, b2 := f.unassigned(t, "First"), f.unassigned(t, "Second")
		dep := f.create(t, func(in *domain.NewTaskInput) { in.Watchers = nil })
		f.link(t, dep, b1)
		f.link(t, dep, b2)
		skip := len(f.notifications(t, dep.ID))

		f.setStatus(t, b1, domain.StatusArchived)
		if got := f.get(t, dep.ID); got.Status != domain.StatusBlocked {
			t.Fatalf("dependent with one open blocker is %s", got.Status)
		}
		if ev := f.eventTypes(t, dep.ID, skip); len(ev) != 0 {
			t.Errorf("events after the first blocker = %v", ev)
		}
		f.setStatus(t, b2, domain.StatusCompleted)
		if got := f.get(t, dep.ID); got.Status != domain.StatusTodo {
			t.Errorf("dependent after both blockers is %s", got.Status)
		}
		if ev := f.eventTypes(t, dep.ID, skip); !slices.Equal(ev, []string{domain.EventTaskUnblocked}) {
			t.Errorf("events = %v, want one task.unblocked", ev)
		}
	})
}

func TestManualBlockSurvivesResolution(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		blocker := f.unassigned(t, "Blocker")
		dep := f.create(t, func(in *domain.NewTaskInput) { in.Watchers = nil })
		dep = f.setStatus(t, dep, domain.StatusBlocked) // manual
		f.link(t, dep, blocker)
		skip := len(f.notifications(t, dep.ID))

		f.setStatus(t, blocker, domain.StatusCompleted)
		got := f.get(t, dep.ID)
		if got.Status != domain.StatusBlocked || *got.BlockedReason != domain.BlockedManual || got.Version != dep.Version {
			t.Errorf("a manual block was changed: %s %v v%d", got.Status, *got.BlockedReason, got.Version)
		}
		if ev := f.eventTypes(t, dep.ID, skip); !slices.Equal(ev, []string{domain.EventTaskDependencyResolved}) {
			t.Errorf("events = %v, want one task.dependency_resolved", ev)
		}
		if cs := f.comments(t, dep.ID); len(cs) != 1 || cs[0].AuthorID != domain.System.ID {
			t.Errorf("comments = %+v, want one system comment", cs)
		}
	})
}

func TestStartedDependentHearsResolution(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		blocker := f.unassigned(t, "Blocker")
		dep := f.create(t, func(in *domain.NewTaskInput) { in.Watchers = nil })
		dep = f.setStatus(t, dep, domain.StatusInProgress)
		f.link(t, dep, blocker)
		skip := len(f.notifications(t, dep.ID))

		f.setStatus(t, blocker, domain.StatusCompleted)
		if got := f.get(t, dep.ID); got.Status != domain.StatusInProgress || got.Version != dep.Version {
			t.Errorf("started dependent changed: %s v%d", got.Status, got.Version)
		}
		if ev := f.eventTypes(t, dep.ID, skip); !slices.Equal(ev, []string{domain.EventTaskDependencyResolved}) {
			t.Errorf("events = %v", ev)
		}
	})
}

func TestRestoringBlockerReopensDependencies(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		blocker := f.unassigned(t, "Blocker")
		waiting := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Waiting"; in.Watchers = nil })
		started := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Started"; in.Watchers = nil })
		done := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Done"; in.Watchers = nil })
		for _, d := range []*domain.Task{waiting, started, done} {
			f.link(t, d, blocker)
		}
		f.setStatus(t, blocker, domain.StatusArchived) // all three unblocked
		started = f.setStatus(t, started, domain.StatusInProgress)
		done = f.setStatus(t, done, domain.StatusCompleted)
		skips := map[uuid.UUID]int{}
		for _, d := range []*domain.Task{waiting, started, done} {
			skips[d.ID] = len(f.notifications(t, d.ID))
		}

		if _, err := f.svc.RestoreTask(ctx, alice, tenant, blocker.ID, nil); err != nil {
			t.Fatal(err)
		}
		if got := f.get(t, waiting.ID); got.Status != domain.StatusBlocked || *got.BlockedReason != domain.BlockedDependency {
			t.Errorf("todo dependent after restore: %s", got.Status)
		}
		if ev := f.eventTypes(t, waiting.ID, skips[waiting.ID]); !slices.Equal(ev, []string{domain.EventTaskBlocked}) {
			t.Errorf("todo dependent events = %v", ev)
		}
		for _, d := range []*domain.Task{started, done} {
			got := f.get(t, d.ID)
			if got.Status != d.Status || got.Version != d.Version {
				t.Errorf("%s dependent changed to %s", d.Status, got.Status)
			}
			ns := f.notifications(t, d.ID)[skips[d.ID]:]
			if len(ns) != 1 || ns[0].EventType != domain.EventTaskDependencyReopened || ns[0].PriorityHint != "high" {
				t.Errorf("%s dependent notifications = %+v", d.Status, ns)
			}
			if cs := f.comments(t, d.ID); len(cs) != 1 || cs[0].AuthorID != domain.System.ID {
				t.Errorf("%s dependent comments = %d", d.Status, len(cs))
			}
		}
	})
}

func TestReopeningBlockerReblocksTodo(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		blocker := f.unassigned(t, "Blocker")
		dep := f.unassigned(t, "Dependent")
		f.link(t, dep, blocker)
		f.setStatus(t, blocker, domain.StatusCompleted)
		f.setStatus(t, blocker, domain.StatusInProgress) // reopen
		if got := f.get(t, dep.ID); got.Status != domain.StatusBlocked || *got.BlockedReason != domain.BlockedDependency {
			t.Errorf("dependent after reopen: %s", got.Status)
		}
		// completed -> archived keeps the dependency resolved: no change.
		f.setStatus(t, blocker, domain.StatusCompleted)
		v := f.get(t, dep.ID).Version
		f.setStatus(t, blocker, domain.StatusArchived)
		if got := f.get(t, dep.ID); got.Status != domain.StatusTodo || got.Version != v {
			t.Errorf("archiving a completed blocker changed the dependent: %s v%d", got.Status, got.Version)
		}
	})
}

func TestDeletingBlockerUnblocksDependents(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		blocker, other := f.unassigned(t, "Blocker"), f.unassigned(t, "Other")
		only := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Only"; in.Watchers = nil })
		both := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Both"; in.Watchers = nil })
		f.link(t, only, blocker)
		f.link(t, both, blocker)
		f.link(t, both, other)
		f.link(t, blocker, other) // the blocker's own links go too

		if err := f.svc.DeleteTask(ctx, tenant, blocker.ID); err != nil {
			t.Fatal(err)
		}
		if got := f.get(t, only.ID); got.Status != domain.StatusTodo {
			t.Errorf("dependent of a deleted blocker is %s", got.Status)
		}
		ns := f.notifications(t, only.ID)
		if last := ns[len(ns)-1]; last.EventType != domain.EventTaskUnblocked || last.Payload["blockerStatus"] != "deleted" {
			t.Errorf("last notification = %+v", last)
		}
		if got := f.get(t, both.ID); got.Status != domain.StatusBlocked {
			t.Errorf("dependent with another open blocker is %s", got.Status)
		}
		if blockers, _ := f.b.Deps.ListBlockers(ctx, tenant, both.ID); !slices.Equal(blockers, []uuid.UUID{other.ID}) {
			t.Errorf("blockers after delete = %v", blockers)
		}
		if deps, _ := f.b.Deps.ListDependents(ctx, tenant, other.ID); slices.Contains(deps, blocker.ID) {
			t.Error("the deleted task's own link survived")
		}
	})
}

// TestAddDependencyRaces runs pairs of inserts that would together close a
// cycle. Exactly one of each pair must succeed.
func TestAddDependencyRaces(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		for i := range 15 {
			// Direct: A->B and B->A at once.
			a, b := f.unassigned(t, "A"), f.unassigned(t, "B")
			race(t, i, "direct", f, [2]uuid.UUID{a.ID, b.ID}, [2]uuid.UUID{b.ID, a.ID})

			// Disjoint pairs: with Y->U and V->X in place, X->Y and U->V lock
			// different tasks but together close X->Y->U->V->X.
			x, y, u, v := f.unassigned(t, "X"), f.unassigned(t, "Y"), f.unassigned(t, "U"), f.unassigned(t, "V")
			f.link(t, y, u)
			f.link(t, v, x)
			race(t, i, "disjoint", f, [2]uuid.UUID{x.ID, y.ID}, [2]uuid.UUID{u.ID, v.ID})
		}
	})
}

func race(t *testing.T, i int, name string, f *fixture, first, second [2]uuid.UUID) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for j, pair := range [][2]uuid.UUID{first, second} {
		wg.Go(func() {
			_, errs[j] = f.svc.AddDependency(ctx, tenant, pair[0], pair[1])
		})
	}
	wg.Wait()
	ok, cycles := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrDependencyCycle):
			cycles++
		default:
			t.Errorf("%s race %d: unexpected error %v", name, i, err)
		}
	}
	if ok != 1 || cycles != 1 {
		t.Errorf("%s race %d: %d succeeded and %d were cycles, want 1 and 1", name, i, ok, cycles)
	}
}
