package repotest

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// stored creates a task in the backend and moves it to status through
// legal transitions.
func stored(t *testing.T, b Backend, tenantID string, status domain.Status, mutate func(*domain.NewTaskInput)) *domain.Task {
	t.Helper()
	task := newTask(t, tenantID, mutate)
	create(t, b, task)
	path := map[domain.Status][]domain.Status{
		domain.StatusInProgress: {domain.StatusInProgress},
		domain.StatusBlocked:    {domain.StatusInProgress, domain.StatusBlocked},
		domain.StatusCompleted:  {domain.StatusCompleted},
		domain.StatusArchived:   {domain.StatusArchived},
	}[status]
	for _, s := range path {
		before := task.Version
		if _, err := task.TransitionTo(s, alice, t0); err != nil {
			t.Fatal(err)
		}
		if err := b.Tasks.Update(ctx, task, before); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// TestCountAssigned covers the load query of the digest gatekeeper (M10-01).
func TestCountAssigned(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		other, _ := domain.NewWorkspace(tenant, "ws_other", "Other", t0)
		if err := b.Workspaces.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		critical := func(in *domain.NewTaskInput) { in.Priority = domain.PriorityCritical }
		stored(t, b, tenant, domain.StatusInProgress, critical)
		stored(t, b, tenant, domain.StatusInProgress, func(in *domain.NewTaskInput) { critical(in); in.WorkspaceID = "ws_other" })
		// None of these count for bob.
		stored(t, b, tenant, domain.StatusTodo, critical)
		stored(t, b, tenant, domain.StatusBlocked, critical)
		stored(t, b, tenant, domain.StatusCompleted, critical)
		stored(t, b, tenant, domain.StatusInProgress, func(in *domain.NewTaskInput) { in.Priority = domain.PriorityHigh })
		stored(t, b, tenant, domain.StatusInProgress, func(in *domain.NewTaskInput) { critical(in); in.AssigneeID = "usr_carol" })
		stored(t, b, "tenant_other", domain.StatusInProgress, critical)

		n, err := b.Tasks.CountAssigned(ctx, tenant, "usr_bob", domain.StatusInProgress, domain.PriorityCritical)
		if err != nil || n != 2 {
			t.Errorf("bob's in-progress critical tasks = %d, %v; want 2 across workspaces", n, err)
		}
		if n, _ := b.Tasks.CountAssigned(ctx, tenant, "usr_carol", domain.StatusInProgress, domain.PriorityCritical); n != 1 {
			t.Errorf("carol = %d, want 1", n)
		}
		if n, _ := b.Tasks.CountAssigned(ctx, tenant, "usr_nobody", domain.StatusInProgress, domain.PriorityCritical); n != 0 {
			t.Errorf("nobody = %d, want 0", n)
		}
	})
}

func TestUnblockedAtRoundTrip(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := stored(t, b, tenant, domain.StatusBlocked, nil)
		at := t0.Add(90 * time.Minute)
		if _, err := task.TransitionTo(domain.StatusTodo, alice, at); err != nil {
			t.Fatal(err)
		}
		if err := b.Tasks.Update(ctx, task, task.Version-1); err != nil {
			t.Fatal(err)
		}
		got, err := b.Tasks.Get(ctx, tenant, task.ID)
		if err != nil || got.UnblockedAt == nil || !got.UnblockedAt.Equal(at) || got.UnblockedAt.Location() != time.UTC {
			t.Fatalf("unblocked_at = %v, %v", got.UnblockedAt, err)
		}
		fresh := newTask(t, tenant, nil)
		create(t, b, fresh)
		if got, _ := b.Tasks.Get(ctx, tenant, fresh.ID); got.UnblockedAt != nil {
			t.Errorf("a new task has unblocked_at %v", got.UnblockedAt)
		}
	})
}

// workspace creates a workspace with the digest agent on or off.
func workspace(t *testing.T, b Backend, tenantID, id string, agents bool, disabled ...string) {
	t.Helper()
	w, err := domain.NewWorkspace(tenantID, id, id, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: &agents, DisabledAgents: &disabled}, t0); err != nil {
		t.Fatal(err)
	}
	if err := b.Workspaces.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
}

// awaiting stores a job awaiting approval on task.
func awaiting(t *testing.T, b Backend, task *domain.Task, at time.Time) domain.AgentJob {
	t.Helper()
	j := newJob(t, task, domain.AgentTriage, at)
	j.DedupKey = domain.RequestJobDedupKey(domain.AgentTriage, task.ID, uuid.New()) // several per task
	insertJob(t, b, j)
	claimed, err := b.AgentJobs.Claim(ctx, at, 1, []string{domain.AgentTriage})
	if err != nil || len(claimed) != 1 || claimed[0].ID != j.ID {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	job := claimed[0]
	job.ProposedActions = json.RawMessage(`[{"tool": "UpdateTaskAttributes", "arguments": {}}]`)
	prev := job.UpdatedAt
	if err := job.TransitionTo(domain.JobAwaitingApproval, at); err != nil {
		t.Fatal(err)
	}
	if err := b.AgentJobs.Update(ctx, job, domain.JobRunning, prev); err != nil {
		t.Fatal(err)
	}
	return *job
}

// TestListBriefRecipients covers who the scheduler briefs (M10-02, D40).
func TestListBriefRecipients(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		day := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
		workspace(t, b, tenant, "ws_b", true)
		workspace(t, b, tenant, "ws_a", true)
		workspace(t, b, tenant, "ws_off", false)
		workspace(t, b, tenant, "ws_nodigest", true, domain.AgentDigest)
		workspace(t, b, "tenant_beta", "ws_a", true)
		in := func(ws, assignee string) func(*domain.NewTaskInput) {
			return func(in *domain.NewTaskInput) { in.WorkspaceID, in.AssigneeID = ws, assignee }
		}
		// bob has open work in two workspaces: the first by ID is named.
		stored(t, b, tenant, domain.StatusTodo, in("ws_b", "usr_bob"))
		stored(t, b, tenant, domain.StatusInProgress, in("ws_a", "usr_bob"))
		// carol only in a workspace with agents off, or the digest disabled.
		stored(t, b, tenant, domain.StatusInProgress, in("ws_off", "usr_carol"))
		stored(t, b, tenant, domain.StatusTodo, in("ws_nodigest", "usr_carol"))
		// dave's work is done, blocked, or archived: nothing to report.
		stored(t, b, tenant, domain.StatusCompleted, in("ws_a", "usr_dave"))
		stored(t, b, tenant, domain.StatusBlocked, in("ws_a", "usr_dave"))
		stored(t, b, tenant, domain.StatusArchived, in("ws_a", "usr_dave"))
		// An unassigned open task briefs nobody, not even its creator, alice.
		stored(t, b, tenant, domain.StatusTodo, in("ws_b", ""))
		// A decision waits on erin, who created an unassigned task, and on
		// frank, whose task is completed but has a proposal pending.
		erins := newTask(t, tenant, in("ws_b", ""))
		erins.CreatorID = "usr_erin"
		create(t, b, erins)
		awaiting(t, b, erins, t0)
		frank := stored(t, b, tenant, domain.StatusCompleted, in("ws_b", "usr_frank"))
		awaiting(t, b, frank, t0.Add(time.Second))
		// The same user ID in another tenant is another person.
		stored(t, b, "tenant_beta", domain.StatusTodo, in("ws_a", "usr_bob"))

		got, err := b.Tasks.ListBriefRecipients(ctx, day, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := []domain.BriefRecipient{
			{TenantID: tenant, UserID: "usr_bob", WorkspaceID: "ws_a"},
			{TenantID: tenant, UserID: "usr_erin", WorkspaceID: "ws_b"},
			{TenantID: tenant, UserID: "usr_frank", WorkspaceID: "ws_b"},
			{TenantID: "tenant_beta", UserID: "usr_bob", WorkspaceID: "ws_a"},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("recipients =\n%v\nwant\n%v", got, want)
		}
		if two, _ := b.Tasks.ListBriefRecipients(ctx, day, 2); !slices.Equal(two, want[:2]) {
			t.Errorf("limit 2 = %v", two)
		}

		// A person with today's brief queued is left out, on that day only.
		job, err := domain.NewBriefJob(want[0], day, day)
		if err != nil {
			t.Fatal(err)
		}
		insertJob(t, b, job)
		if got, _ := b.Tasks.ListBriefRecipients(ctx, day.Add(3*time.Hour), 10); !slices.Equal(got, want[1:]) {
			t.Errorf("after bob's brief = %v", got)
		}
		if got, _ := b.Tasks.ListBriefRecipients(ctx, day.Add(24*time.Hour), 10); !slices.Equal(got, want) {
			t.Errorf("the next day = %v", got)
		}
	})
}

func TestListAwaitingApproval(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_beta")
		bobs := stored(t, b, tenant, domain.StatusTodo, nil)
		unassigned := newTask(t, tenant, func(in *domain.NewTaskInput) { in.AssigneeID = "" }) // created by alice
		create(t, b, unassigned)
		carols := stored(t, b, tenant, domain.StatusTodo, func(in *domain.NewTaskInput) { in.AssigneeID = "usr_carol" })
		theirs := stored(t, b, "tenant_beta", domain.StatusTodo, nil)

		second := awaiting(t, b, bobs, t0.Add(2*time.Second))
		first := awaiting(t, b, bobs, t0.Add(time.Second))
		alices := awaiting(t, b, unassigned, t0)
		awaiting(t, b, carols, t0)
		pending := newJob(t, bobs, domain.AgentBlocker, t0) // not yet asking
		insertJob(t, b, pending)
		j := newJob(t, theirs, domain.AgentTriage, t0)
		j.TenantID = "tenant_beta"
		insertJob(t, b, j)

		ids := func(jobs []*domain.AgentJob) []uuid.UUID {
			out := []uuid.UUID{}
			for _, j := range jobs {
				out = append(out, j.ID)
			}
			return out
		}
		// Oldest first by creation time.
		got, err := b.AgentJobs.ListAwaitingApproval(ctx, tenant, "usr_bob")
		if err != nil || !slices.Equal(ids(got), []uuid.UUID{first.ID, second.ID}) {
			t.Errorf("bob's approvals = %v, %v; want %v", ids(got), err, []uuid.UUID{first.ID, second.ID})
		}
		if got, _ := b.AgentJobs.ListAwaitingApproval(ctx, tenant, "usr_alice"); !slices.Equal(ids(got), []uuid.UUID{alices.ID}) {
			t.Errorf("alice (creator of an unassigned task) = %v", ids(got))
		}
		if got, _ := b.AgentJobs.ListAwaitingApproval(ctx, "tenant_beta", "usr_bob"); len(got) != 0 {
			t.Errorf("bob in another tenant = %v", ids(got))
		}
	})
}
