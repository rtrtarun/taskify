package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/migrations"
)

var (
	ctx    = context.Background()
	t0     = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	tenant = "tenant_acme"
	ws     = "ws_core"
	alice  = domain.User("usr_alice")
)

// each runs fn once per backend, each time on a fresh store.
func each(t *testing.T, fn func(t *testing.T, b Backend)) {
	t.Helper()
	for _, f := range Factories(t) {
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			fn(t, f.New(t))
		})
	}
}

func seedWorkspace(t *testing.T, b Backend, tenantID string) {
	t.Helper()
	w, err := domain.NewWorkspace(tenantID, ws, "Core", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Workspaces.Create(ctx, w); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
}

func newTask(t *testing.T, tenantID string, mutate func(*domain.NewTaskInput)) *domain.Task {
	t.Helper()
	in := domain.NewTaskInput{TenantID: tenantID, WorkspaceID: ws, Title: "Fix checkout crash", AssigneeID: "usr_bob"}
	if mutate != nil {
		mutate(&in)
	}
	task, _, err := domain.NewTask(in, alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func create(t *testing.T, b Backend, task *domain.Task) {
	t.Helper()
	if err := b.Tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestWorkspaces(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		got, err := b.Workspaces.Get(ctx, tenant, ws)
		if err != nil || got.Name != "Core" || !got.CreatedAt.Equal(t0) {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		w, _ := domain.NewWorkspace(tenant, ws, "Again", t0)
		if err := b.Workspaces.Create(ctx, w); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("duplicate: error = %v, want ErrAlreadyExists", err)
		}
		if _, err := b.Workspaces.Get(ctx, "tenant_other", ws); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("other tenant: error = %v, want ErrNotFound", err)
		}
	})
}

func TestTaskRoundTrip(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		due := t0.Add(48 * time.Hour)
		task := newTask(t, tenant, func(in *domain.NewTaskInput) {
			in.Description = "Steps to reproduce"
			in.Priority = domain.PriorityHigh
			in.Labels = []string{"bug", "backend"}
			in.Metadata = map[string]any{"source": "slack", "count": 3.0}
			in.Watchers = []string{"usr_dave", "usr_carol"}
			in.DueAt = &due
		})
		create(t, b, task)

		got, err := b.Tasks.Get(ctx, tenant, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != task.Title || got.Description != task.Description || got.Priority != domain.PriorityHigh ||
			got.Status != domain.StatusTodo || got.AssigneeID != "usr_bob" || got.CreatorID != alice.ID ||
			got.WorkspaceID != ws || got.Version != 1 {
			t.Errorf("scalar fields differ: %+v", got)
		}
		if !slices.Equal(got.Labels, []string{"bug", "backend"}) {
			t.Errorf("Labels = %v", got.Labels)
		}
		if !slices.Equal(got.Watchers, []string{"usr_carol", "usr_dave"}) {
			t.Errorf("Watchers = %v", got.Watchers)
		}
		if got.Metadata["source"] != "slack" || got.Metadata["count"] != 3.0 {
			t.Errorf("Metadata = %v", got.Metadata)
		}
		if got.DueAt == nil || !got.DueAt.Equal(due) || !got.CreatedAt.Equal(t0) || got.CreatedAt.Location() != time.UTC {
			t.Errorf("times: due %v created %v", got.DueAt, got.CreatedAt)
		}
		if got.CompletedAt != nil || got.ArchivedAt != nil || got.BlockedReason != nil {
			t.Errorf("unexpected non-nil optional fields")
		}

		// A task with no assignee, labels, or watchers reads back as empty, not nil.
		bare := newTask(t, tenant, func(in *domain.NewTaskInput) { in.AssigneeID = "" })
		create(t, b, bare)
		got, err = b.Tasks.Get(ctx, tenant, bare.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.AssigneeID != "" || got.Labels == nil || got.Watchers == nil || got.Metadata == nil {
			t.Errorf("empty fields: %+v", got)
		}

		if err := b.Tasks.Create(ctx, task); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("duplicate create: error = %v", err)
		}
	})
}

func TestTaskTenantIsolation(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		task := newTask(t, tenant, nil)
		create(t, b, task)

		if _, err := b.Tasks.Get(ctx, "tenant_other", task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("cross-tenant Get: error = %v, want ErrNotFound", err)
		}
		foreign := *task
		foreign.TenantID = "tenant_other"
		foreign.Title = "hijacked"
		if err := b.Tasks.Update(ctx, &foreign, 1); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("cross-tenant Update: error = %v, want ErrNotFound", err)
		}
		page, err := b.Tasks.List(ctx, repository.TaskFilter{TenantID: "tenant_other", Limit: 10})
		if err != nil || len(page.Tasks) != 0 {
			t.Errorf("cross-tenant List = %d tasks, %v", len(page.Tasks), err)
		}
		if err := b.Tasks.MarkOverdueNotified(ctx, "tenant_other", task.ID, t0); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("cross-tenant mark: error = %v", err)
		}
	})
}

func TestTaskWorkspaceMustExist(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		err := b.Tasks.Create(ctx, newTask(t, tenant, nil))
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("error = %v, want ErrValidation", err)
		}
	})
}

func TestTaskUpdate(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, func(in *domain.NewTaskInput) { in.Watchers = []string{"usr_carol"} })
		create(t, b, task)

		if _, err := task.Apply(domain.Patch{
			Title:    ptr("Renamed"),
			Status:   ptr(domain.StatusBlocked),
			Watchers: ptr([]string{"usr_erin", "usr_dave"}),
		}, alice, t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := b.Tasks.Update(ctx, task, 1); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := b.Tasks.Get(ctx, tenant, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "Renamed" || got.Version != 2 || got.Status != domain.StatusBlocked ||
			*got.BlockedReason != domain.BlockedManual || *got.StatusBeforeBlocked != domain.StatusTodo ||
			!got.UpdatedAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("after update: %+v", got)
		}
		if !slices.Equal(got.Watchers, []string{"usr_dave", "usr_erin"}) {
			t.Errorf("Watchers = %v", got.Watchers)
		}

		// The same write again carries a stale expected version.
		if err := b.Tasks.Update(ctx, task, 1); !errors.Is(err, domain.ErrVersionConflict) {
			t.Errorf("stale update: error = %v, want ErrVersionConflict", err)
		}
		missing := newTask(t, tenant, nil)
		if err := b.Tasks.Update(ctx, missing, 1); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing update: error = %v, want ErrNotFound", err)
		}
	})
}

func TestTransactions(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		boom := errors.New("boom")

		err := b.Tx.WithTx(ctx, func(ctx context.Context) error {
			if err := b.Tasks.Create(ctx, task); err != nil {
				return err
			}
			entries := outboxEntries(t, task, 1)
			if _, err := b.Outbox.Insert(ctx, entries); err != nil {
				return err
			}
			// Nested WithTx joins the outer transaction.
			return b.Tx.WithTx(ctx, func(context.Context) error { return boom })
		})
		if !errors.Is(err, boom) {
			t.Fatalf("WithTx error = %v", err)
		}
		if _, err := b.Tasks.Get(ctx, tenant, task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("task survived a rolled-back transaction: %v", err)
		}
		if n, _ := b.Outbox.CountByStatus(ctx, domain.OutboxPending); n != 0 {
			t.Errorf("outbox rows survived a rolled-back transaction: %d", n)
		}

		if _, err := b.Tasks.GetForUpdate(ctx, tenant, task.ID); !errors.Is(err, repository.ErrTxRequired) {
			t.Errorf("GetForUpdate outside a transaction: error = %v", err)
		}
		if _, err := b.Tasks.ClaimOverdue(ctx, t0, 1); !errors.Is(err, repository.ErrTxRequired) {
			t.Errorf("ClaimOverdue outside a transaction: error = %v", err)
		}

		create(t, b, task)
		err = b.Tx.WithTx(ctx, func(ctx context.Context) error {
			got, err := b.Tasks.GetForUpdate(ctx, tenant, task.ID)
			if err == nil && got.ID != task.ID {
				return fmt.Errorf("wrong task")
			}
			return err
		})
		if err != nil {
			t.Errorf("GetForUpdate in a transaction: %v", err)
		}
	})
}

func TestListFiltersAndPagination(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		var all []*domain.Task
		for i := 0; i < 7; i++ {
			task := newTask(t, tenant, func(in *domain.NewTaskInput) {
				in.Title = fmt.Sprintf("task %d", i)
				if i%2 == 0 {
					in.AssigneeID = "usr_carol"
					in.Priority = domain.PriorityCritical
				}
			})
			// Distinct update times, with two sharing one to exercise the ID tie-break.
			task.UpdatedAt = t0.Add(time.Duration(i/2*2+i%2) * time.Minute)
			if i == 6 {
				task.UpdatedAt = t0.Add(5 * time.Minute)
			}
			create(t, b, task)
			all = append(all, task)
		}
		archived := newTask(t, tenant, func(in *domain.NewTaskInput) { in.Title = "archived" })
		if _, err := archived.TransitionTo(domain.StatusArchived, alice, t0); err != nil {
			t.Fatal(err)
		}
		create(t, b, archived)

		// Page through everything, two at a time.
		var seen []uuid.UUID
		var cursor *repository.Cursor
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("pagination did not terminate")
			}
			page, err := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant, Limit: 2, After: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range page.Tasks {
				seen = append(seen, task.ID)
			}
			if page.Next == nil {
				break
			}
			cursor = page.Next
		}
		if len(seen) != 7 {
			t.Fatalf("paged through %d tasks, want 7 (archived excluded)", len(seen))
		}
		want := slices.Clone(all)
		slices.SortFunc(want, func(a, b *domain.Task) int {
			if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
				return c
			}
			return -slices.Compare(a.ID[:], b.ID[:])
		})
		for i, task := range want {
			if seen[i] != task.ID {
				t.Fatalf("order differs at %d", i)
			}
		}

		count := func(f repository.TaskFilter) int {
			f.TenantID, f.Limit = tenant, 100
			page, err := b.Tasks.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			return len(page.Tasks)
		}
		if n := count(repository.TaskFilter{AssigneeID: "usr_carol"}); n != 4 {
			t.Errorf("assignee filter = %d, want 4", n)
		}
		if n := count(repository.TaskFilter{Priority: domain.PriorityCritical}); n != 4 {
			t.Errorf("priority filter = %d, want 4", n)
		}
		if n := count(repository.TaskFilter{Status: domain.StatusArchived}); n != 1 {
			t.Errorf("explicit archived filter = %d, want 1", n)
		}
		if n := count(repository.TaskFilter{Status: domain.StatusTodo, AssigneeID: "usr_bob"}); n != 3 {
			t.Errorf("combined filter = %d, want 3", n)
		}
		if _, err := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("zero limit: error = %v", err)
		}
	})
}

// TestPaginationStableUnderInserts: rows created while a client pages sort
// ahead of its cursor, so pages already started neither repeat nor skip rows.
func TestPaginationStableUnderInserts(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		var original []uuid.UUID
		for i := 0; i < 6; i++ {
			task := newTask(t, tenant, nil)
			task.UpdatedAt = t0.Add(time.Duration(i) * time.Minute)
			create(t, b, task)
			original = append(original, task.ID)
		}
		page, err := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uuid.UUID]int{}
		for _, task := range page.Tasks {
			seen[task.ID]++
		}
		for i := 0; i < 3; i++ {
			late := newTask(t, tenant, nil)
			late.UpdatedAt = t0.Add(time.Hour + time.Duration(i)*time.Minute)
			create(t, b, late)
		}
		next, err := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant, Limit: 10, After: page.Next})
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range next.Tasks {
			seen[task.ID]++
		}
		if len(seen) != 6 {
			t.Fatalf("saw %d distinct tasks, want the 6 original", len(seen))
		}
		for _, id := range original {
			if seen[id] != 1 {
				t.Errorf("task seen %d times", seen[id])
			}
		}
	})
}

func TestDeadlineClaims(t *testing.T) {
	now := t0.Add(100 * time.Hour)
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		mk := func(name string, due time.Duration, mutate func(*domain.Task)) *domain.Task {
			d := now.Add(due)
			task := newTask(t, tenant, func(in *domain.NewTaskInput) { in.Title = name; in.DueAt = &d })
			if mutate != nil {
				mutate(task)
			}
			create(t, b, task)
			return task
		}
		at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
		completed := func(task *domain.Task) {
			if _, err := task.TransitionTo(domain.StatusCompleted, alice, t0); err != nil {
				t.Fatal(err)
			}
		}

		mk("due in 20h, never reminded", 20*time.Hour, nil)
		mk("due in 30m, never reminded", 30*time.Minute, nil)
		mk("due in 40m, 24h reminder sent earlier", 40*time.Minute, func(x *domain.Task) { x.LastDueSoonNotifiedAt = at(-10 * time.Hour) })
		mk("due in 40m, 1h reminder already sent", 40*time.Minute, func(x *domain.Task) { x.LastDueSoonNotifiedAt = at(-5 * time.Minute) })
		mk("due in 20h, already reminded", 20*time.Hour, func(x *domain.Task) { x.LastDueSoonNotifiedAt = at(-time.Hour) })
		mk("due in 30h", 30*time.Hour, nil)
		mk("due in 20h, completed", 20*time.Hour, completed)

		mk("overdue, never reminded", -time.Hour, nil)
		mk("overdue, reminded yesterday", -48*time.Hour, func(x *domain.Task) { x.LastOverdueNotifiedAt = at(-24 * time.Hour) })
		mk("overdue, reminded today", -48*time.Hour, func(x *domain.Task) { x.LastOverdueNotifiedAt = at(-time.Minute) })
		mk("overdue, completed", -time.Hour, completed)
		mk("overdue, blocked", -time.Hour, func(x *domain.Task) {
			if _, err := x.TransitionTo(domain.StatusBlocked, alice, t0); err != nil {
				t.Fatal(err)
			}
		})

		titles := func(claim func(context.Context, time.Time, int) ([]*domain.Task, error), limit int) []string {
			var out []string
			err := b.Tx.WithTx(ctx, func(ctx context.Context) error {
				tasks, err := claim(ctx, now, limit)
				for _, task := range tasks {
					out = append(out, task.Title)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(out)
			return out
		}

		wantSoon := []string{"due in 20h, never reminded", "due in 30m, never reminded", "due in 40m, 24h reminder sent earlier"}
		slices.Sort(wantSoon)
		if got := titles(b.Tasks.ClaimDueSoon, 100); !slices.Equal(got, wantSoon) {
			t.Errorf("ClaimDueSoon = %v\nwant %v", got, wantSoon)
		}
		wantOver := []string{"overdue, blocked", "overdue, never reminded", "overdue, reminded yesterday"}
		if got := titles(b.Tasks.ClaimOverdue, 100); !slices.Equal(got, wantOver) {
			t.Errorf("ClaimOverdue = %v\nwant %v", got, wantOver)
		}
		if got := titles(b.Tasks.ClaimOverdue, 2); len(got) != 2 {
			t.Errorf("limit not applied: %v", got)
		}
	})
}

func TestComments(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		create(t, b, task)
		for i := 0; i < 4; i++ {
			c, _, err := domain.NewComment(task, fmt.Sprintf("comment %d @usr_carol", i), alice, t0.Add(time.Duration(i)*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Comments.Create(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		got, err := b.Comments.ListRecent(ctx, task.ID, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[0].Body != "comment 3 @usr_carol" || got[2].Body != "comment 1 @usr_carol" {
			t.Fatalf("ListRecent = %+v", got)
		}
		if !slices.Equal(got[0].Mentions, []string{"usr_carol"}) || got[0].AuthorID != alice.ID {
			t.Errorf("comment fields = %+v", got[0])
		}
		orphan, _, _ := domain.NewComment(newTask(t, tenant, nil), "orphan", alice, t0)
		if err := b.Comments.Create(ctx, orphan); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("comment on a missing task: error = %v", err)
		}
	})
}

// outboxEntries returns n pending entries for task with distinct keys.
func outboxEntries(t *testing.T, task *domain.Task, n int) []domain.OutboxEntry {
	t.Helper()
	var out []domain.OutboxEntry
	for i := 0; i < n; i++ {
		id, _ := uuid.NewV7()
		out = append(out, domain.OutboxEntry{
			ID: id, TenantID: task.TenantID, AggregateID: task.ID, EventType: domain.EventTaskUpdated,
			IdempotencyKey: fmt.Sprintf("taskify:%s:%s:%d", task.TenantID, task.ID, i),
			Payload:        json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
			MaxAttempts:    10, NextAttemptAt: t0, CreatedAt: t0.Add(time.Duration(i) * time.Millisecond),
		})
	}
	return out
}

func TestOutboxLifecycle(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		entries := outboxEntries(t, task, 3)

		n, err := b.Outbox.Insert(ctx, entries)
		if err != nil || n != 3 {
			t.Fatalf("Insert = %d, %v", n, err)
		}
		dup := entries[0]
		dup.ID = uuid.New()
		if n, err := b.Outbox.Insert(ctx, []domain.OutboxEntry{dup}); err != nil || n != 0 {
			t.Errorf("duplicate key: inserted %d, err %v", n, err)
		}

		// Rows due later are not claimed.
		late := outboxEntries(t, newTask(t, tenant, nil), 1)[0]
		late.NextAttemptAt = t0.Add(time.Hour)
		if _, err := b.Outbox.Insert(ctx, []domain.OutboxEntry{late}); err != nil {
			t.Fatal(err)
		}

		claimed, err := b.Outbox.ClaimBatch(ctx, t0.Add(time.Second), 10)
		if err != nil || len(claimed) != 3 {
			t.Fatalf("ClaimBatch = %d rows, %v", len(claimed), err)
		}
		for _, e := range claimed {
			if e.Status != domain.OutboxProcessing || e.ClaimedAt == nil || string(e.Payload) == "" {
				t.Errorf("claimed row = %+v", e)
			}
		}
		if again, _ := b.Outbox.ClaimBatch(ctx, t0.Add(time.Second), 10); len(again) != 0 {
			t.Errorf("claimed rows were claimed again: %d", len(again))
		}

		ok, retry, dead := claimed[0], claimed[1], claimed[2]
		if err := b.Outbox.MarkDispatched(ctx, ok.ID, *ok.ClaimedAt, t0.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := b.Outbox.MarkRetry(ctx, retry.ID, *retry.ClaimedAt, 1, t0.Add(time.Minute), "503 from AgentPulse"); err != nil {
			t.Fatal(err)
		}
		if err := b.Outbox.MarkDeadLetter(ctx, dead.ID, *dead.ClaimedAt, 1, "400 bad request"); err != nil {
			t.Fatal(err)
		}

		got, _ := b.Outbox.Get(ctx, ok.ID)
		if got.Status != domain.OutboxDispatched || got.DispatchedAt == nil || got.ClaimedAt != nil {
			t.Errorf("dispatched row = %+v", got)
		}
		got, _ = b.Outbox.Get(ctx, retry.ID)
		if got.Status != domain.OutboxPending || got.Attempts != 1 || got.LastError != "503 from AgentPulse" || !got.NextAttemptAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("retried row = %+v", got)
		}
		got, _ = b.Outbox.Get(ctx, dead.ID)
		if got.Status != domain.OutboxDeadLetter || got.Attempts != 1 {
			t.Errorf("dead-letter row = %+v", got)
		}

		// A settled claim cannot be settled again with the old token.
		if err := b.Outbox.MarkDeadLetter(ctx, ok.ID, *ok.ClaimedAt, 9, "late"); err != nil {
			t.Errorf("stale settle returned an error: %v", err)
		}
		if got, _ := b.Outbox.Get(ctx, ok.ID); got.Status != domain.OutboxDispatched {
			t.Errorf("stale claim overwrote a dispatched row: %s", got.Status)
		}
		// Settling a row that no longer exists is not an error.
		if err := b.Outbox.MarkDispatched(ctx, uuid.New(), t0, t0); err != nil {
			t.Errorf("settling a missing row: %v", err)
		}

		// Release keeps the attempt count.
		if early, _ := b.Outbox.ClaimBatch(ctx, t0.Add(30*time.Second), 10); len(early) != 0 {
			t.Fatalf("claimed %d rows before their retry time", len(early))
		}
		reclaimed, _ := b.Outbox.ClaimBatch(ctx, t0.Add(2*time.Hour), 10)
		if len(reclaimed) != 2 { // the retried row and the late row
			t.Fatalf("reclaimed %d rows, want 2", len(reclaimed))
		}
		for _, e := range reclaimed {
			if err := b.Outbox.Release(ctx, e.ID, *e.ClaimedAt, t0.Add(3*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := b.Outbox.Get(ctx, retry.ID); got.Attempts != 1 || got.Status != domain.OutboxPending {
			t.Errorf("released row = %+v", got)
		}

		list, err := b.Outbox.ListForAggregate(ctx, task.ID)
		if err != nil || len(list) != 3 {
			t.Errorf("ListForAggregate = %d, %v", len(list), err)
		}
		if n, _ := b.Outbox.CountByStatus(ctx, domain.OutboxPending); n != 2 {
			t.Errorf("pending count = %d, want 2", n)
		}
		if _, err := b.Outbox.Get(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Get missing: error = %v", err)
		}
	})
}

func TestOutboxLeasesAndCleanup(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		task := newTask(t, tenant, nil)
		if _, err := b.Outbox.Insert(ctx, outboxEntries(t, task, 4)); err != nil {
			t.Fatal(err)
		}
		first, _ := b.Outbox.ClaimBatch(ctx, t0, 2)
		second, _ := b.Outbox.ClaimBatch(ctx, t0.Add(2*time.Minute), 2)
		if len(first) != 2 || len(second) != 2 {
			t.Fatalf("claims = %d, %d", len(first), len(second))
		}

		// Only the older claim's lease has expired.
		n, err := b.Outbox.ReleaseExpiredLeases(ctx, t0.Add(time.Minute))
		if err != nil || n != 2 {
			t.Fatalf("ReleaseExpiredLeases = %d, %v", n, err)
		}
		// The abandoned worker's late result is ignored once another claim takes the row.
		retaken, _ := b.Outbox.ClaimBatch(ctx, t0.Add(3*time.Minute), 10)
		if len(retaken) != 2 {
			t.Fatalf("retaken = %d", len(retaken))
		}
		if err := b.Outbox.MarkDeadLetter(ctx, first[0].ID, *first[0].ClaimedAt, 1, "stale worker"); err != nil {
			t.Fatal(err)
		}
		if got, _ := b.Outbox.Get(ctx, first[0].ID); got.Status != domain.OutboxProcessing {
			t.Errorf("stale worker changed a reclaimed row to %s", got.Status)
		}

		for _, e := range append(second, retaken...) {
			if err := b.Outbox.MarkDispatched(ctx, e.ID, *e.ClaimedAt, t0.Add(time.Duration(len(e.IdempotencyKey))*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		// Everything was dispatched at least t0+10s; delete in batches of 3.
		cutoff := t0.Add(24 * time.Hour)
		n1, _ := b.Outbox.DeleteDispatchedBefore(ctx, cutoff, 3)
		n2, _ := b.Outbox.DeleteDispatchedBefore(ctx, cutoff, 3)
		if n1 != 3 || n2 != 1 {
			t.Errorf("deleted %d then %d, want 3 then 1", n1, n2)
		}
		if n, _ := b.Outbox.DeleteDispatchedBefore(ctx, t0, 10); n != 0 {
			t.Errorf("deleted rows newer than the cutoff: %d", n)
		}
	})
}

// TestConcurrentClaimsNeverOverlap is the core guarantee behind
// multi-replica relays: no row is handed to two claimers.
func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		task := newTask(t, tenant, nil)
		const rows = 300
		if _, err := b.Outbox.Insert(ctx, outboxEntries(t, task, rows)); err != nil {
			t.Fatal(err)
		}
		var (
			mu    sync.Mutex
			seen  = map[uuid.UUID]int{}
			wg    sync.WaitGroup
			errCh = make(chan error, 8)
		)
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					batch, err := b.Outbox.ClaimBatch(ctx, t0, 7)
					if err != nil {
						errCh <- err
						return
					}
					if len(batch) == 0 {
						return
					}
					mu.Lock()
					for _, e := range batch {
						seen[e.ID]++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
		if len(seen) != rows {
			t.Fatalf("claimed %d distinct rows, want %d", len(seen), rows)
		}
		for _, n := range seen {
			if n != 1 {
				t.Fatalf("a row was claimed %d times", n)
			}
		}
	})
}

// TestConcurrentDeadlineClaimsNeverOverlap: two transactions scanning at once
// each lock a disjoint set of tasks. PostgreSQL only: the memory store runs
// transactions one at a time, so there is never a concurrent scan to test.
func TestConcurrentDeadlineClaimsNeverOverlap(t *testing.T) {
	b := Postgres(t)
	seedWorkspace(t, b, tenant)
	for i := 0; i < 20; i++ {
		due := t0.Add(-time.Hour)
		create(t, b, newTask(t, tenant, func(in *domain.NewTaskInput) { in.DueAt = &due }))
	}
	var (
		wg      sync.WaitGroup
		ready   sync.WaitGroup
		mu      sync.Mutex
		seen    = map[uuid.UUID]int{}
		release = make(chan struct{})
	)
	for w := 0; w < 2; w++ {
		wg.Add(1)
		ready.Add(1)
		go func() {
			defer wg.Done()
			err := b.Tx.WithTx(ctx, func(ctx context.Context) error {
				tasks, err := b.Tasks.ClaimOverdue(ctx, t0, 12)
				mu.Lock()
				for _, task := range tasks {
					seen[task.ID]++
				}
				mu.Unlock()
				ready.Done()
				<-release // hold the row locks until both have claimed
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	ready.Wait()
	close(release)
	wg.Wait()
	if len(seen) != 20 {
		t.Errorf("claimed %d distinct tasks, want 20", len(seen))
	}
	for _, n := range seen {
		if n != 1 {
			t.Errorf("a task was claimed %d times", n)
		}
	}
}

func TestMigrationsRoundTrip(t *testing.T) {
	pool := NewEmptyDatabase(t)
	exec := func(ctx context.Context, sql string) error {
		_, err := pool.Exec(ctx, sql)
		return err
	}
	tables := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i, d := range []migrations.Direction{migrations.Up, migrations.Down, migrations.Up} {
		if err := migrations.Apply(ctx, exec, d); err != nil {
			t.Fatalf("step %d (%s): %v", i, d, err)
		}
	}
	if n := tables(); n != 14 { // 000007 adds user_contacts and webhook_callbacks, 000008 learning_suggestions, 000010 notification_acks
		t.Errorf("tables after up/down/up = %d, want 14", n)
	}
	if err := migrations.Apply(ctx, exec, migrations.Down); err != nil {
		t.Fatal(err)
	}
	if n := tables(); n != 0 {
		t.Errorf("tables after down = %d, want 0", n)
	}
}

// TestAgentSchemaOnR1Data applies the R1 migrations, writes R1 rows, then runs
// 000004 up, down, and up again (task M5-01).
func TestAgentSchemaOnR1Data(t *testing.T) {
	pool := NewEmptyDatabase(t)
	run := func(name string) {
		t.Helper()
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"000001_init_tasks.up.sql", "000002_outbox.up.sql", "000003_integrations.up.sql"} {
		run(name)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspaces (tenant_id, id, name) VALUES ('tenant_acme', 'ws_core', 'Core');
		INSERT INTO tasks (tenant_id, workspace_id, title, creator_id) VALUES ('tenant_acme', 'ws_core', 'r1 task', 'u1');
		INSERT INTO tasks (tenant_id, workspace_id, title, creator_id, status, blocked_reason, status_before_blocked)
			VALUES ('tenant_acme', 'ws_core', 'r1 blocked', 'u1', 'blocked', 'manual', 'todo');`); err != nil {
		t.Fatal(err)
	}
	run("000004_agent_schema.up.sql")
	run("000004_agent_schema.down.sql")
	run("000004_agent_schema.up.sql")

	var tasks int
	var settings string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE agent_metadata = '{}'::jsonb AND parent_id IS NULL`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 2 {
		t.Errorf("R1 tasks after migration = %d, want 2", tasks)
	}
	if err := pool.QueryRow(ctx, `SELECT agent_settings->>'agents_enabled' FROM workspaces WHERE id = 'ws_core'`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if settings != "false" {
		t.Errorf("existing workspace agents_enabled = %s, want false", settings)
	}
}

func TestAgentSchemaConstraints(t *testing.T) {
	b := Postgres(t)
	seedWorkspace(t, b, tenant)
	pool := b.DB.Pool
	exec := func(sql string, args ...any) error { _, err := pool.Exec(ctx, sql, args...); return err }
	newTask := func() string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO tasks (tenant_id, workspace_id, title, creator_id) VALUES ('tenant_acme','ws_core','x','u') RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, c := newTask(), newTask()

	if err := exec(`INSERT INTO task_dependencies (task_id, depends_on_task_id, tenant_id) VALUES ($1, $1, 'tenant_acme')`, a); err == nil {
		t.Error("a self-dependency was accepted")
	}
	if err := exec(`UPDATE tasks SET parent_id = id WHERE id = $1`, a); err == nil {
		t.Error("a task was made its own parent")
	}
	if err := exec(`UPDATE tasks SET estimated_minutes = 0 WHERE id = $1`, a); err == nil {
		t.Error("a zero estimate was accepted")
	}
	if err := exec(`INSERT INTO task_dependencies (task_id, depends_on_task_id, tenant_id) VALUES ($1, $2, 'tenant_acme')`, a, c); err != nil {
		t.Fatalf("a valid dependency was rejected: %v", err)
	}
	if err := exec(`UPDATE tasks SET parent_id = $1 WHERE id = $2`, a, c); err != nil {
		t.Fatal(err)
	}
	job := `INSERT INTO agent_jobs (tenant_id, task_id, agent_type, dedup_key) VALUES ('tenant_acme', $1, 'triage', $2)`
	if err := exec(job, a, "triage:"+a+":v1"); err != nil {
		t.Fatalf("a valid job was rejected: %v", err)
	}
	if err := exec(job, a, "triage:"+a+":v1"); err == nil {
		t.Error("a duplicate dedup_key was accepted")
	}
	if err := exec(`INSERT INTO agent_jobs (tenant_id, agent_type, dedup_key, decided_by) VALUES ('tenant_acme', 'digest', 'd1', 'u')`); err == nil {
		t.Error("decided_by without decided_at was accepted")
	}

	var defaults string
	if err := pool.QueryRow(ctx, `SELECT agent_settings::text FROM workspaces WHERE id = 'ws_core'`).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"agents_enabled": false`, `"require_hitl_for_all": true`, `"max_subtasks_per_decomposition": 5`} {
		if !strings.Contains(defaults, want) {
			t.Errorf("default agent_settings %s lacks %s", defaults, want)
		}
	}

	// Deleting the blocker cascades its dependency row and the job; deleting
	// the parent detaches the child rather than deleting it.
	if err := exec(`DELETE FROM tasks WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	var deps, jobs, children int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM task_dependencies`).Scan(&deps)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM agent_jobs WHERE task_id IS NOT NULL`).Scan(&jobs)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id = $1 AND parent_id IS NULL`, c).Scan(&children)
	if deps != 0 || jobs != 0 || children != 1 {
		t.Errorf("after delete: deps=%d jobs=%d detached child=%d, want 0 0 1", deps, jobs, children)
	}
}

func TestSchemaConstraints(t *testing.T) {
	b := Postgres(t)
	seedWorkspace(t, b, tenant)
	pool := b.DB.Pool
	exec := func(sql string) error { _, err := pool.Exec(ctx, sql); return err }

	if err := exec(`INSERT INTO tasks (tenant_id, workspace_id, title, creator_id, status) VALUES ('tenant_acme','ws_core','x','u','blocked')`); err == nil {
		t.Error("a blocked task without a reason was accepted")
	}
	if err := exec(`INSERT INTO tasks (tenant_id, workspace_id, title, creator_id, blocked_reason, status_before_blocked) VALUES ('tenant_acme','ws_core','x','u','manual','todo')`); err == nil {
		t.Error("a block reason on an unblocked task was accepted")
	}
	if err := exec(`INSERT INTO tasks (tenant_id, workspace_id, title, creator_id, status, blocked_reason, status_before_blocked) VALUES ('tenant_acme','ws_core','x','u','blocked','bored','todo')`); err == nil {
		t.Error("an unknown block reason was accepted")
	}
	if err := exec(`INSERT INTO tasks (tenant_id, workspace_id, title, creator_id, status, blocked_reason, status_before_blocked) VALUES ('tenant_acme','ws_core','x','u','blocked','manual','todo')`); err != nil {
		t.Errorf("a valid blocked task was rejected: %v", err)
	}
	if err := exec(`DELETE FROM workspaces WHERE id = 'ws_core'`); err == nil {
		t.Error("a workspace with tasks was deleted")
	}
}

func TestTaskDelete(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, func(in *domain.NewTaskInput) { in.Watchers = []string{"usr_carol"} })
		create(t, b, task)
		c, _, _ := domain.NewComment(task, "hello", alice, t0)
		if err := b.Comments.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Outbox.Insert(ctx, outboxEntries(t, task, 2)); err != nil {
			t.Fatal(err)
		}

		if err := b.Tasks.Delete(ctx, "tenant_other", task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("cross-tenant delete: error = %v", err)
		}
		if n, err := b.Outbox.DeleteForAggregate(ctx, "tenant_other", task.ID); err != nil || n != 0 {
			t.Errorf("cross-tenant outbox delete removed %d rows, %v", n, err)
		}
		if n, err := b.Outbox.DeleteForAggregate(ctx, tenant, task.ID); err != nil || n != 2 {
			t.Errorf("DeleteForAggregate = %d, %v", n, err)
		}
		if err := b.Tasks.Delete(ctx, tenant, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Tasks.Get(ctx, tenant, task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("task still readable: %v", err)
		}
		if comments, _ := b.Comments.ListRecent(ctx, task.ID, 10); len(comments) != 0 {
			t.Errorf("comments survived the delete")
		}
		if err := b.Tasks.Delete(ctx, tenant, task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("second delete: error = %v", err)
		}
		if b.DB != nil {
			var watchers int
			if err := b.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM task_watchers`).Scan(&watchers); err != nil || watchers != 0 {
				t.Errorf("watchers left = %d, %v", watchers, err)
			}
		}
	})
}

func TestWorkspaceUpdateAndRules(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		w, _ := b.Workspaces.Get(ctx, tenant, ws)
		if err := w.Rename("Core Platform", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := b.Workspaces.Update(ctx, w); err != nil {
			t.Fatal(err)
		}
		got, _ := b.Workspaces.Get(ctx, tenant, ws)
		if got.Name != "Core Platform" || !got.UpdatedAt.Equal(t0.Add(time.Hour)) || !got.CreatedAt.Equal(t0) {
			t.Errorf("after update: %+v", got)
		}
		if !got.AgentSettings.RequireHITLForAll || got.AgentSettings.AgentsEnabled || got.AgentSettings.MaxSubtasksPerDecomposition != 5 {
			t.Errorf("default agent settings = %+v", got.AgentSettings)
		}
		on, disabled := true, []string{domain.AgentDigest}
		if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: &on, DisabledAgents: &disabled}, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := b.Workspaces.Update(ctx, w); err != nil {
			t.Fatal(err)
		}
		disabled[0] = "mutated after save"
		got, _ = b.Workspaces.Get(ctx, tenant, ws)
		if !got.AgentSettings.Allows(domain.AgentTriage) || got.AgentSettings.Allows(domain.AgentDigest) || !got.AgentSettings.RequireHITLForAll {
			t.Errorf("agent settings after update = %+v", got.AgentSettings)
		}
		missing := w
		missing.ID = "nope"
		if err := b.Workspaces.Update(ctx, missing); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("update missing: error = %v", err)
		}
		if _, err := b.Workspaces.GetForUpdate(ctx, tenant, ws); !errors.Is(err, repository.ErrTxRequired) {
			t.Errorf("GetForUpdate outside a transaction: error = %v", err)
		}
		err := b.Tx.WithTx(ctx, func(ctx context.Context) error {
			_, err := b.Workspaces.GetForUpdate(ctx, tenant, ws)
			return err
		})
		if err != nil {
			t.Errorf("GetForUpdate in a transaction: %v", err)
		}

		for _, key := range []string{"low-priority-digest", "critical-or-blocked"} {
			r := domain.WorkspaceRule{TenantID: tenant, WorkspaceID: ws, Key: key, AgentPulseRuleID: "ap_" + key, CreatedAt: t0}
			if err := b.Rules.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		dup := domain.WorkspaceRule{TenantID: tenant, WorkspaceID: ws, Key: "critical-or-blocked", AgentPulseRuleID: "x", CreatedAt: t0}
		if err := b.Rules.Create(ctx, dup); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("duplicate rule: error = %v", err)
		}
		orphan := domain.WorkspaceRule{TenantID: tenant, WorkspaceID: "nope", Key: "k", AgentPulseRuleID: "x", CreatedAt: t0}
		if err := b.Rules.Create(ctx, orphan); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("rule for a missing workspace: error = %v", err)
		}
		rules, err := b.Rules.List(ctx, tenant, ws)
		if err != nil || len(rules) != 2 || rules[0].Key != "critical-or-blocked" || rules[0].AgentPulseRuleID != "ap_critical-or-blocked" {
			t.Errorf("List = %+v, %v", rules, err)
		}
		if other, _ := b.Rules.List(ctx, "tenant_other", ws); len(other) != 0 {
			t.Errorf("another tenant sees %d rules", len(other))
		}
	})
}

func TestWebhookDeliveries(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		fresh, err := b.Deliveries.Record(ctx, "sig-1", t0)
		if err != nil || !fresh {
			t.Fatalf("first record = %v, %v", fresh, err)
		}
		if fresh, _ := b.Deliveries.Record(ctx, "sig-1", t0.Add(time.Second)); fresh {
			t.Error("a repeated key was reported as new")
		}
		if _, err := b.Deliveries.Record(ctx, "sig-2", t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n, err := b.Deliveries.DeleteBefore(ctx, t0.Add(time.Hour)); err != nil || n != 1 {
			t.Errorf("DeleteBefore = %d, %v", n, err)
		}
		if fresh, _ := b.Deliveries.Record(ctx, "sig-1", t0.Add(3*time.Hour)); !fresh {
			t.Error("a key past retention was not accepted again")
		}
	})
}
