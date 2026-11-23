package repotest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// The list reads the clients need (M12-04), on both backends.

func TestListChildren(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		parent := newTask(t, tenant, nil)
		create(t, b, parent)
		var children []*domain.Task
		for i := 0; i < 3; i++ {
			c := newTask(t, tenant, nil)
			c.ParentID = &parent.ID
			c.CreatedAt = t0.Add(time.Duration(3-i) * time.Minute) // created in reverse order
			c.UpdatedAt = c.CreatedAt
			create(t, b, c)
			children = append(children, c)
		}
		create(t, b, newTask(t, tenant, nil)) // not a child

		got, err := b.Tasks.ListChildren(ctx, tenant, parent.ID)
		if err != nil || len(got) != 3 {
			t.Fatalf("ListChildren = %d, %v", len(got), err)
		}
		for i, want := range []*domain.Task{children[2], children[1], children[0]} {
			if got[i].ID != want.ID || got[i].ParentID == nil || *got[i].ParentID != parent.ID {
				t.Errorf("child %d = %s, want %s (oldest first)", i, got[i].ID, want.ID)
			}
		}
		if got, err := b.Tasks.ListChildren(ctx, "tenant_other", parent.ID); err != nil || len(got) != 0 {
			t.Errorf("another tenant's children = %d, %v", len(got), err)
		}
		if got, err := b.Tasks.ListChildren(ctx, tenant, children[0].ID); err != nil || got == nil || len(got) != 0 {
			t.Errorf("a task without children = %v, %v", got, err)
		}
	})
}

func TestTaskListIDPrefix(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		var tasks []*domain.Task
		for i := 0; i < 3; i++ {
			task := newTask(t, tenant, nil)
			create(t, b, task)
			tasks = append(tasks, task)
		}
		foreign := newTask(t, "tenant_other", nil)
		create(t, b, foreign)
		list := func(tenantID, prefix string) []*domain.Task {
			t.Helper()
			page, err := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenantID, IDPrefix: prefix, Limit: 50})
			if err != nil {
				t.Fatalf("List(%q): %v", prefix, err)
			}
			return page.Tasks
		}
		id := tasks[1].ID.String()
		for _, prefix := range []string{id, strings.ToUpper(id), id[:35]} {
			if got := list(tenant, prefix); len(got) != 1 || got[0].ID != tasks[1].ID {
				t.Errorf("prefix %q matched %d tasks", prefix, len(got))
			}
		}
		// A short prefix matches every task whose ID starts with it.
		short := list(tenant, id[:8])
		found := false
		for _, task := range short {
			found = found || task.ID == tasks[1].ID
			if !strings.HasPrefix(task.ID.String(), id[:8]) {
				t.Errorf("prefix %q matched %s", id[:8], task.ID)
			}
		}
		if !found {
			t.Errorf("prefix %q missed %s", id[:8], id)
		}
		if got := list(tenant, foreign.ID.String()); len(got) != 0 {
			t.Errorf("another tenant's task matched: %d", len(got))
		}
		if got := list(tenant, "f"+id[1:9]); len(got) != 0 {
			t.Errorf("a prefix no task has matched %d", len(got))
		}
	})
}

func TestAgentJobList(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		task, other := newTask(t, tenant, nil), newTask(t, tenant, nil)
		create(t, b, task)
		create(t, b, other)
		insert := func(tenantID string, taskID *uuid.UUID, agent string, at time.Time, status domain.JobStatus) *domain.AgentJob {
			t.Helper()
			j, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenantID, TaskID: taskID, AgentType: agent,
				DedupKey: uuid.NewString()}, at)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.AgentJobs.Insert(ctx, j); err != nil {
				t.Fatal(err)
			}
			if status != domain.JobPending {
				prev := j.UpdatedAt
				j.Status, j.UpdatedAt = status, at.Add(time.Second)
				if err := b.AgentJobs.Update(ctx, &j, domain.JobPending, prev); err != nil {
					t.Fatal(err)
				}
			}
			return &j
		}
		var want []*domain.AgentJob // newest first
		for i := 0; i < 5; i++ {
			status := domain.JobPending
			if i%2 == 1 {
				status = domain.JobRunning
			}
			want = append([]*domain.AgentJob{insert(tenant, &task.ID, domain.AgentTriage, t0.Add(time.Duration(i)*time.Minute), status)}, want...)
		}
		decomposer := insert(tenant, &other.ID, domain.AgentDecomposer, t0.Add(time.Hour), domain.JobPending)
		brief := insert(tenant, nil, domain.AgentDigest, t0.Add(2*time.Hour), domain.JobPending)
		otherTask := newTask(t, "tenant_other", nil)
		create(t, b, otherTask)
		insert("tenant_other", &otherTask.ID, domain.AgentTriage, t0, domain.JobPending)

		list := func(f repository.AgentJobFilter) repository.AgentJobPage {
			t.Helper()
			f.TenantID = tenant
			page, err := b.AgentJobs.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			return page
		}
		all := list(repository.AgentJobFilter{Limit: 50})
		if len(all.Jobs) != 7 || all.Next != nil || all.Jobs[0].ID != brief.ID || all.Jobs[1].ID != decomposer.ID {
			t.Fatalf("all = %d jobs, next %v", len(all.Jobs), all.Next)
		}
		// Paging through the task's triage jobs, two at a time.
		var seen []uuid.UUID
		f := repository.AgentJobFilter{TaskID: &task.ID, Limit: 2}
		for {
			page := list(f)
			for _, j := range page.Jobs {
				seen = append(seen, j.ID)
			}
			if page.Next == nil {
				break
			}
			f.After = page.Next
		}
		if len(seen) != 5 {
			t.Fatalf("paged %d jobs, want 5", len(seen))
		}
		for i, j := range want {
			if seen[i] != j.ID {
				t.Errorf("page order: job %d = %s, want %s", i, seen[i], j.ID)
			}
		}
		if got := list(repository.AgentJobFilter{Status: domain.JobRunning, Limit: 50}); len(got.Jobs) != 2 {
			t.Errorf("running = %d, want 2", len(got.Jobs))
		}
		if got := list(repository.AgentJobFilter{AgentType: domain.AgentDigest, Limit: 50}); len(got.Jobs) != 1 || got.Jobs[0].TaskID != nil {
			t.Errorf("digest = %+v", got.Jobs)
		}
		if got := list(repository.AgentJobFilter{AgentType: domain.AgentDecomposer, TaskID: &task.ID, Limit: 50}); len(got.Jobs) != 0 {
			t.Errorf("filters do not combine: %d", len(got.Jobs))
		}
		if _, err := b.AgentJobs.List(ctx, repository.AgentJobFilter{TenantID: tenant}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("no limit: %v", err)
		}
	})
}

func TestOutboxListAndReplay(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		task := newTask(t, tenant, nil)
		create(t, b, task)
		entries := outboxEntries(t, task, 4)
		foreignTask := newTask(t, "tenant_other", nil)
		create(t, b, foreignTask)
		foreign := outboxEntries(t, foreignTask, 1)
		if _, err := b.Outbox.Insert(ctx, append(entries, foreign...)); err != nil {
			t.Fatal(err)
		}
		// Kill the first and the foreign row.
		claimed, err := b.Outbox.ClaimBatch(ctx, t0.Add(time.Second), 10)
		if err != nil || len(claimed) != 5 {
			t.Fatalf("ClaimBatch = %d, %v", len(claimed), err)
		}
		for _, e := range claimed {
			switch e.ID {
			case entries[0].ID, foreign[0].ID:
				err = b.Outbox.MarkDeadLetter(ctx, e.ID, *e.ClaimedAt, 10, "400 bad request")
			default:
				err = b.Outbox.Release(ctx, e.ID, *e.ClaimedAt, t0)
			}
			if err != nil {
				t.Fatal(err)
			}
		}

		list := func(f repository.OutboxFilter) repository.OutboxPage {
			t.Helper()
			f.TenantID = tenant
			page, err := b.Outbox.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			return page
		}
		all := list(repository.OutboxFilter{Limit: 3})
		if len(all.Entries) != 3 || all.Next == nil || all.Entries[0].ID != entries[3].ID {
			t.Fatalf("first page = %d rows, next %v", len(all.Entries), all.Next)
		}
		rest := list(repository.OutboxFilter{Limit: 3, After: all.Next})
		if len(rest.Entries) != 1 || rest.Entries[0].ID != entries[0].ID || rest.Next != nil {
			t.Fatalf("second page = %+v", rest)
		}
		dead := list(repository.OutboxFilter{Status: domain.OutboxDeadLetter, Limit: 50})
		if len(dead.Entries) != 1 || dead.Entries[0].ID != entries[0].ID || dead.Entries[0].LastError != "400 bad request" ||
			string(dead.Entries[0].Payload) == "" {
			t.Fatalf("dead letters = %+v", dead.Entries)
		}

		now := t0.Add(time.Hour)
		got, err := b.Outbox.Replay(ctx, tenant, entries[0].ID, now)
		if err != nil || got.Status != domain.OutboxPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(now) || got.ClaimedAt != nil {
			t.Fatalf("Replay = %+v, %v", got, err)
		}
		if again, err := b.Outbox.ClaimBatch(ctx, now, 10); err != nil || !containsEntry(again, entries[0].ID) {
			t.Errorf("the replayed row was not claimed: %v", err)
		}
		for name, tc := range map[string]struct {
			tenantID string
			id       uuid.UUID
			want     error
		}{
			"pending row":       {tenant, entries[1].ID, domain.ErrNotDeadLetter},
			"processing row":    {tenant, entries[0].ID, domain.ErrNotDeadLetter},
			"another tenant's":  {tenant, foreign[0].ID, domain.ErrNotFound},
			"unknown":           {tenant, uuid.New(), domain.ErrNotFound},
			"from other tenant": {"tenant_other", entries[1].ID, domain.ErrNotFound},
		} {
			if _, err := b.Outbox.Replay(ctx, tc.tenantID, tc.id, now); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
		if e, _ := b.Outbox.Get(ctx, foreign[0].ID); e.Status != domain.OutboxDeadLetter {
			t.Errorf("another tenant's row = %s", e.Status)
		}
		if _, err := b.Outbox.List(ctx, repository.OutboxFilter{TenantID: tenant}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("no limit: %v", err)
		}
	})
}

func containsEntry(entries []domain.OutboxEntry, id uuid.UUID) bool {
	for _, e := range entries {
		if e.ID == id {
			return true
		}
	}
	return false
}

// TestTaskListStateAndDelta covers what the polling client needs (M14-02):
// the count and latest update of a list, updated_since, and the tasks
// archived since a time.
func TestTaskListStateAndDelta(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
		mk := func(tenantID string, minutes int, mutate func(*domain.Task)) *domain.Task {
			task := newTask(t, tenantID, nil)
			task.CreatedAt, task.UpdatedAt = at(minutes), at(minutes)
			if mutate != nil {
				mutate(task)
			}
			create(t, b, task)
			return task
		}
		mk(tenant, 1, nil)
		mk(tenant, 5, nil)
		archivedEarly := mk(tenant, 2, func(x *domain.Task) {
			x.Status, x.ArchivedAt = domain.StatusArchived, ptr(at(2))
		})
		archivedLate := mk(tenant, 8, func(x *domain.Task) {
			x.Status, x.ArchivedAt, x.AssigneeID = domain.StatusArchived, ptr(at(8)), "usr_carol"
		})
		mk("tenant_other", 9, nil)

		f := repository.TaskFilter{TenantID: tenant, Limit: 1}
		st, err := b.Tasks.ListState(ctx, f)
		if err != nil || st.Count != 2 || !st.Latest.Equal(at(5)) || st.ArchivedSince != 0 {
			t.Fatalf("ListState = %+v, %v (the limit must not matter, archived tasks are not counted)", st, err)
		}
		if st, err = b.Tasks.ListState(ctx, repository.TaskFilter{TenantID: "tenant_none"}); err != nil || st.Count != 0 || !st.Latest.IsZero() {
			t.Errorf("empty ListState = %+v, %v", st, err)
		}

		since := at(3)
		f.UpdatedSince = &since
		st, err = b.Tasks.ListState(ctx, f)
		if err != nil || st.Count != 1 || !st.Latest.Equal(at(5)) || st.ArchivedSince != 1 {
			t.Fatalf("delta ListState = %+v, %v", st, err)
		}
		page, err := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant, UpdatedSince: &since, Limit: 10})
		if err != nil || len(page.Tasks) != 1 || !page.Tasks[0].UpdatedAt.Equal(at(5)) {
			t.Fatalf("delta List = %d tasks, %v", len(page.Tasks), err)
		}

		ids, err := b.Tasks.ArchivedSince(ctx, f)
		if err != nil || len(ids) != 1 || ids[0] != archivedLate.ID {
			t.Fatalf("ArchivedSince = %v, %v", ids, err)
		}
		older := at(0)
		ids, _ = b.Tasks.ArchivedSince(ctx, repository.TaskFilter{TenantID: tenant, UpdatedSince: &older})
		if len(ids) != 2 || ids[0] != archivedEarly.ID || ids[1] != archivedLate.ID {
			t.Errorf("ArchivedSince from the start = %v (oldest first)", ids)
		}
		ids, _ = b.Tasks.ArchivedSince(ctx, repository.TaskFilter{TenantID: tenant, UpdatedSince: &older, AssigneeID: "usr_carol"})
		if len(ids) != 1 || ids[0] != archivedLate.ID {
			t.Errorf("ArchivedSince for one assignee = %v", ids)
		}
		if ids, err = b.Tasks.ArchivedSince(ctx, repository.TaskFilter{TenantID: "tenant_other", UpdatedSince: &older}); err != nil || ids == nil || len(ids) != 0 {
			t.Errorf("another tenant's ArchivedSince = %v, %v", ids, err)
		}
		if ids, err = b.Tasks.ArchivedSince(ctx, repository.TaskFilter{TenantID: tenant}); err != nil || ids == nil || len(ids) != 0 {
			t.Errorf("ArchivedSince without a time = %v, %v", ids, err)
		}
	})
}

func TestWorkspaceList(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		for _, c := range []struct{ tenant, id string }{
			{tenant, "ws_b"}, {tenant, "ws_a"}, {tenant, "ws_C"}, {"tenant_other", "ws_x"},
		} {
			w, err := domain.NewWorkspace(c.tenant, c.id, c.id, t0)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Workspaces.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
		}
		ids := func(after string, limit int) []string {
			got, err := b.Workspaces.List(ctx, tenant, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, w := range got {
				out = append(out, w.ID)
			}
			return out
		}
		// Byte order: upper case sorts before lower case, on both stores.
		if got := ids("", 10); !slices.Equal(got, []string{"ws_C", "ws_a", "ws_b"}) {
			t.Errorf("all = %v", got)
		}
		if got := ids("", 2); !slices.Equal(got, []string{"ws_C", "ws_a"}) {
			t.Errorf("limit = %v", got)
		}
		if got := ids("ws_a", 10); !slices.Equal(got, []string{"ws_b"}) {
			t.Errorf("after = %v", got)
		}
	})
}
