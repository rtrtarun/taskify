package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

var (
	ctx    = context.Background()
	t0     = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	tenant = "tenant_acme"
	alice  = domain.User("usr_alice")
	bob    = domain.User("usr_bob")
)

func ptr[T any](v T) *T { return &v }

type fixture struct {
	b     repotest.Backend
	svc   *TaskService
	wakes atomic.Int32
	now   time.Time
}

// each runs fn once per backend with a service whose workspace ws_core exists.
func each(t *testing.T, fn func(t *testing.T, f *fixture)) {
	t.Helper()
	for _, factory := range repotest.Factories(t) {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			fn(t, newFixture(t, factory.New(t), nil))
		})
	}
}

func newFixture(t *testing.T, b repotest.Backend, outbox repository.OutboxRepository) *fixture {
	t.Helper()
	f := &fixture{b: b, now: t0}
	if outbox == nil {
		outbox = b.Outbox
	}
	f.svc = NewTaskService(
		Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments, Outbox: outbox},
		Config{PublicBaseURL: "https://taskify.test", OutboxMaxAttempts: 7},
		WithClock(func() time.Time { return f.now }),
		WithWake(func() { f.wakes.Add(1) }),
	)
	// Fixtures may share a store, so the workspace can already exist.
	if _, err := f.svc.CreateWorkspace(ctx, tenant, "ws_core", "Core"); err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) create(t *testing.T, mutate func(*domain.NewTaskInput)) *domain.Task {
	t.Helper()
	in := domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "Fix checkout crash", AssigneeID: bob.ID, Watchers: []string{"usr_carol"}}
	if mutate != nil {
		mutate(&in)
	}
	task, err := f.svc.CreateTask(ctx, alice, in)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// outbox returns the event types and recipients written for a task.
func (f *fixture) outbox(t *testing.T, taskID uuid.UUID) []string {
	t.Helper()
	entries, err := f.b.Outbox.ListForAggregate(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		var n domain.Notification
		if err := json.Unmarshal(e.Payload, &n); err != nil {
			t.Fatal(err)
		}
		if e.MaxAttempts != 7 {
			t.Errorf("max attempts = %d, want the configured 7", e.MaxAttempts)
		}
		out = append(out, e.EventType+">"+n.UserID)
	}
	slices.Sort(out)
	return out
}

func TestCreateTask(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		if task.Version != 1 || task.CreatorID != alice.ID || !task.CreatedAt.Equal(t0) {
			t.Errorf("task = %+v", task)
		}
		stored, err := f.b.Tasks.Get(ctx, tenant, task.ID)
		if err != nil || stored.Title != task.Title {
			t.Fatalf("stored = %+v, %v", stored, err)
		}
		if got := f.outbox(t, task.ID); !slices.Equal(got, []string{"task.created>usr_bob"}) {
			t.Errorf("outbox = %v", got)
		}
		if f.wakes.Load() != 1 {
			t.Errorf("wakes = %d, want 1", f.wakes.Load())
		}

		// A task assigned to its creator notifies nobody and does not wake the relay.
		self := f.create(t, func(in *domain.NewTaskInput) { in.AssigneeID = alice.ID; in.Watchers = nil })
		if got := f.outbox(t, self.ID); len(got) != 0 {
			t.Errorf("self-assigned outbox = %v", got)
		}
		if f.wakes.Load() != 1 {
			t.Errorf("wakes = %d, want still 1", f.wakes.Load())
		}
	})
}

func TestCreateTaskValidation(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		_, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_missing", Title: "x"})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("unknown workspace: error = %v, want ErrValidation", err)
		}
		_, err = f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_other", WorkspaceID: "ws_core", Title: "x"})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("another tenant's workspace: error = %v, want ErrValidation", err)
		}
		_, err = f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: ""})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("empty title: error = %v", err)
		}
		if _, err := f.svc.CreateWorkspace(ctx, tenant, "ws_core", "Dup"); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("duplicate workspace: error = %v", err)
		}
		if _, err := f.svc.CreateWorkspace(ctx, tenant, "bad id", "x"); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("bad workspace id: error = %v", err)
		}
	})
}

// failingOutbox fails every insert, standing in for a broken outbox table.
type failingOutbox struct{ repository.OutboxRepository }

var errOutbox = errors.New("outbox insert failed")

func (failingOutbox) Insert(context.Context, []domain.OutboxEntry) (int, error) { return 0, errOutbox }

// TestOutboxFailureRollsBackTheChange is the M2 exit criterion: a task change
// and its notification commit together or not at all.
func TestOutboxFailureRollsBackTheChange(t *testing.T) {
	for _, factory := range repotest.Factories(t) {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			b := factory.New(t)
			good := newFixture(t, b, nil)
			bad := newFixture(t, b, failingOutbox{b.Outbox}) // same store, broken outbox

			_, err := bad.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "doomed", AssigneeID: bob.ID})
			if !errors.Is(err, errOutbox) {
				t.Fatalf("create: error = %v", err)
			}
			page, _ := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant, Limit: 10})
			if len(page.Tasks) != 0 {
				t.Fatalf("a task was saved although its outbox insert failed")
			}

			task := good.create(t, nil)
			_, err = bad.svc.UpdateTask(ctx, alice, tenant, task.ID, 1, domain.Patch{Title: ptr("Renamed")})
			if !errors.Is(err, errOutbox) {
				t.Fatalf("update: error = %v", err)
			}
			stored, _ := b.Tasks.Get(ctx, tenant, task.ID)
			if stored.Title != "Fix checkout crash" || stored.Version != 1 {
				t.Errorf("update survived a failed outbox insert: %q v%d", stored.Title, stored.Version)
			}

			_, err = bad.svc.AddComment(ctx, alice, tenant, task.ID, "hello")
			if !errors.Is(err, errOutbox) {
				t.Fatalf("comment: error = %v", err)
			}
			if comments, _ := b.Comments.ListRecent(ctx, task.ID, 10); len(comments) != 0 {
				t.Errorf("comment survived a failed outbox insert")
			}
			if bad.wakes.Load() != 0 {
				t.Errorf("relay woken after a rolled-back transaction")
			}
		})
	}
}

func TestUpdateTask(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		f.now = t0.Add(time.Minute)

		updated, err := f.svc.UpdateTask(ctx, alice, tenant, task.ID, 1, domain.Patch{
			Priority: ptr(domain.PriorityCritical),
			Status:   ptr(domain.StatusBlocked),
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Version != 2 || !updated.UpdatedAt.Equal(f.now) {
			t.Errorf("updated = v%d at %v", updated.Version, updated.UpdatedAt)
		}
		want := []string{
			"task.blocked>usr_bob", "task.blocked>usr_carol",
			"task.created>usr_bob",
			"task.priority_escalated>usr_bob", "task.priority_escalated>usr_carol",
		}
		if got := f.outbox(t, task.ID); !slices.Equal(got, want) {
			t.Errorf("outbox = %v\nwant %v", got, want)
		}

		// A stale version is refused with the current version, and nothing is written.
		_, err = f.svc.UpdateTask(ctx, alice, tenant, task.ID, 1, domain.Patch{Title: ptr("Stale")})
		var conflict *VersionConflictError
		if !errors.As(err, &conflict) || conflict.Current != 2 || !errors.Is(err, domain.ErrVersionConflict) {
			t.Fatalf("stale update: error = %v", err)
		}
		if stored, _ := f.b.Tasks.Get(ctx, tenant, task.ID); stored.Title != "Fix checkout crash" {
			t.Errorf("stale update was applied")
		}

		// A patch that changes nothing writes nothing.
		before := len(f.outbox(t, task.ID))
		same, err := f.svc.UpdateTask(ctx, alice, tenant, task.ID, 2, domain.Patch{Title: ptr("Fix checkout crash")})
		if err != nil || same.Version != 2 {
			t.Fatalf("no-op update = v%d, %v", same.Version, err)
		}
		if len(f.outbox(t, task.ID)) != before {
			t.Error("a no-op update wrote outbox rows")
		}

		// A disallowed transition changes nothing.
		_, err = f.svc.UpdateTask(ctx, alice, tenant, task.ID, 2, domain.Patch{Status: ptr(domain.StatusCompleted)})
		if !errors.Is(err, domain.ErrInvalidStateTransition) {
			t.Errorf("blocked to completed: error = %v", err)
		}

		if _, err := f.svc.UpdateTask(ctx, alice, "tenant_other", task.ID, 2, domain.Patch{Title: ptr("x")}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("cross-tenant update: error = %v", err)
		}
		if _, err := f.svc.UpdateTask(ctx, alice, tenant, uuid.New(), 1, domain.Patch{Title: ptr("x")}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing task: error = %v", err)
		}
	})
}

func TestCompleteArchiveRestore(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)

		// "Mark done" on a todo task, without a version, yields one task.completed.
		done, err := f.svc.CompleteTask(ctx, bob, tenant, task.ID, nil)
		if err != nil || done.Status != domain.StatusCompleted || done.CompletedAt == nil {
			t.Fatalf("complete = %+v, %v", done, err)
		}
		completed := 0
		for _, e := range f.outbox(t, task.ID) {
			if e == "task.completed>usr_alice" || e == "task.completed>usr_carol" {
				completed++
			}
		}
		if completed != 2 {
			t.Errorf("task.completed rows = %d, want 2 (creator and watcher)", completed)
		}
		// A repeated click is harmless.
		again, err := f.svc.CompleteTask(ctx, bob, tenant, task.ID, nil)
		if err != nil || again.Version != done.Version {
			t.Errorf("second complete = v%d, %v", again.Version, err)
		}

		if _, err := f.svc.RestoreTask(ctx, alice, tenant, task.ID, nil); !errors.Is(err, domain.ErrInvalidStateTransition) {
			t.Errorf("restore of a completed task: error = %v", err)
		}
		archived, err := f.svc.ArchiveTask(ctx, alice, tenant, task.ID, &done.Version)
		if err != nil || archived.Status != domain.StatusArchived || archived.ArchivedAt == nil {
			t.Fatalf("archive = %+v, %v", archived, err)
		}

		// Archived: absent from lists, still fetchable, restorable.
		page, err := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant})
		if err != nil || len(page.Tasks) != 0 {
			t.Errorf("list after archive = %d tasks, %v", len(page.Tasks), err)
		}
		if detail, err := f.svc.GetTask(ctx, tenant, task.ID); err != nil || detail.Task.Status != domain.StatusArchived {
			t.Errorf("get archived = %v", err)
		}
		restored, err := f.svc.RestoreTask(ctx, alice, tenant, task.ID, nil)
		if err != nil || restored.Status != domain.StatusTodo || restored.ArchivedAt != nil || restored.CompletedAt != nil {
			t.Fatalf("restore = %+v, %v", restored, err)
		}
		if page, _ := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant}); len(page.Tasks) != 1 {
			t.Errorf("list after restore = %d tasks", len(page.Tasks))
		}
		if _, err := f.svc.ArchiveTask(ctx, alice, tenant, task.ID, ptr(1)); !errors.Is(err, domain.ErrVersionConflict) {
			t.Errorf("archive with a stale version: error = %v", err)
		}
	})
}

func TestAddComment(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		c, err := f.svc.AddComment(ctx, alice, tenant, task.ID, "@usr_dave can you pair with @usr_bob?")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(c.Mentions, []string{"usr_dave", "usr_bob"}) {
			t.Errorf("Mentions = %v", c.Mentions)
		}
		entries, _ := f.b.Outbox.ListForAggregate(ctx, task.ID)
		mentioned := map[string]bool{}
		for _, e := range entries {
			if e.EventType != domain.EventTaskCommentAdded {
				continue
			}
			var n domain.Notification
			_ = json.Unmarshal(e.Payload, &n)
			mentioned[n.UserID] = n.Payload["isMentioned"] == true
		}
		want := map[string]bool{"usr_bob": true, "usr_carol": false, "usr_dave": true}
		if len(mentioned) != len(want) {
			t.Fatalf("comment recipients = %v", mentioned)
		}
		for u, m := range want {
			if mentioned[u] != m {
				t.Errorf("%s isMentioned = %v, want %v", u, mentioned[u], m)
			}
		}
		if stored, _ := f.b.Tasks.Get(ctx, tenant, task.ID); stored.Version != 1 {
			t.Errorf("a comment changed the version to %d", stored.Version)
		}

		detail, err := f.svc.GetTask(ctx, tenant, task.ID)
		if err != nil || len(detail.Comments) != 1 || detail.Comments[0].ID != c.ID {
			t.Errorf("GetTask comments = %+v, %v", detail.Comments, err)
		}
		if _, err := f.svc.AddComment(ctx, alice, "tenant_other", task.ID, "hi"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("cross-tenant comment: error = %v", err)
		}
		if _, err := f.svc.AddComment(ctx, alice, tenant, task.ID, "  "); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("empty comment: error = %v", err)
		}
	})
}

func TestGetAndList(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		if _, err := f.svc.GetTask(ctx, tenant, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing task: error = %v", err)
		}
		task := f.create(t, nil)
		detail, err := f.svc.GetTask(ctx, tenant, task.ID)
		if err != nil || detail.Comments == nil {
			t.Errorf("detail = %+v, %v", detail, err)
		}
		for i := 0; i < 3; i++ {
			f.now = f.now.Add(time.Second)
			f.create(t, nil)
		}
		page, err := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant, Limit: 3})
		if err != nil || len(page.Tasks) != 3 || page.Next == nil {
			t.Errorf("page = %d tasks, next %v, %v", len(page.Tasks), page.Next, err)
		}
		page, _ = f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant, Limit: 10_000})
		if len(page.Tasks) != 4 {
			t.Errorf("capped list = %d", len(page.Tasks))
		}
		empty, err := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: "tenant_none"})
		if err != nil || empty.Tasks == nil || len(empty.Tasks) != 0 {
			t.Errorf("empty list = %v, %v", empty.Tasks, err)
		}
		if _, err := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant, Status: "done"}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("bad status filter: error = %v", err)
		}
		if _, err := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant, Priority: "p9"}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("bad priority filter: error = %v", err)
		}
	})
}

// TestConcurrentEditsOneWins: two writers holding the same version race;
// exactly one commits and the other gets a conflict.
func TestConcurrentEditsOneWins(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		const writers = 6
		var (
			wg        sync.WaitGroup
			ok        atomic.Int32
			conflicts atomic.Int32
		)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := f.svc.UpdateTask(ctx, alice, tenant, task.ID, 1, domain.Patch{Description: ptr(string(rune('a' + i)))})
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, domain.ErrVersionConflict):
					conflicts.Add(1)
				default:
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		if ok.Load() != 1 || conflicts.Load() != writers-1 {
			t.Errorf("successes %d, conflicts %d", ok.Load(), conflicts.Load())
		}
		if stored, _ := f.b.Tasks.Get(ctx, tenant, task.ID); stored.Version != 2 {
			t.Errorf("version = %d, want 2", stored.Version)
		}
	})
}
