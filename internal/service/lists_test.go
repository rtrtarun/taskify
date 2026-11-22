package service

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// The reads behind the list endpoints (M12-04).

func TestListSubtasks(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		parent := f.create(t, nil)
		var children []*domain.Task
		for i := 0; i < 2; i++ {
			c, _, err := domain.NewTask(domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "Step"}, alice,
				t0.Add(time.Duration(i)*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			c.ParentID = &parent.ID
			if err := f.b.Tasks.Create(ctx, c); err != nil {
				t.Fatal(err)
			}
			children = append(children, c)
		}
		if _, err := f.b.Deps.Create(ctx, domain.Dependency{TaskID: children[1].ID, DependsOnTaskID: children[0].ID, TenantID: tenant, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
		got, err := f.svc.ListSubtasks(ctx, tenant, parent.ID)
		if err != nil || len(got) != 2 {
			t.Fatalf("ListSubtasks = %d, %v", len(got), err)
		}
		if got[0].Task.ID != children[0].ID || got[0].DependsOn == nil || len(got[0].DependsOn) != 0 ||
			len(got[1].DependsOn) != 1 || got[1].DependsOn[0] != children[0].ID {
			t.Errorf("subtasks = %+v", got)
		}
		if _, err := f.svc.ListSubtasks(ctx, "tenant_other", parent.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("another tenant: %v", err)
		}
		if _, err := f.svc.ListSubtasks(ctx, tenant, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("unknown parent: %v", err)
		}
	})
}

func TestListTasksIDPrefix(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		page, err := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant, IDPrefix: task.ID.String()})
		if err != nil || len(page.Tasks) != 1 {
			t.Errorf("whole ID: %d, %v", len(page.Tasks), err)
		}
		if _, err := f.svc.ListTasks(ctx, repository.TaskFilter{TenantID: tenant, IDPrefix: "nope"}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("bad prefix: %v", err)
		}
	})
}

func TestListAgentJobsAndRuns(t *testing.T) {
	each(t, func(t *testing.T, base *fixture) {
		f := newJobsFixture(t, base)
		task := f.create(t, nil)
		job := f.proposal(t, task, "Described")
		if err := f.b.AgentJobs.InsertRun(ctx, domain.AgentRun{ID: uuid.New(), JobID: job.ID, Turn: 1, Model: "m",
			Outcome: "final", CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}

		page, err := f.jobs.List(ctx, repository.AgentJobFilter{TenantID: tenant, Status: domain.JobAwaitingApproval})
		if err != nil || len(page.Jobs) != 1 || page.Jobs[0].ID != job.ID {
			t.Fatalf("List = %+v, %v", page, err)
		}
		if page, _ := f.jobs.List(ctx, repository.AgentJobFilter{TenantID: tenant, Limit: 1000}); len(page.Jobs) == 0 {
			t.Error("a limit over the maximum listed nothing")
		}
		for name, filter := range map[string]repository.AgentJobFilter{
			"status":     {TenantID: tenant, Status: "done"},
			"agent type": {TenantID: tenant, AgentType: "oracle"},
		} {
			if _, err := f.jobs.List(ctx, filter); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("%s: %v", name, err)
			}
		}

		runs, err := f.jobs.RunsForTask(ctx, tenant, task.ID)
		if err != nil || len(runs) != 1 || runs[0].Job.ID != job.ID || len(runs[0].Runs) != 1 {
			t.Fatalf("RunsForTask = %+v, %v", runs, err)
		}
		other := f.create(t, nil)
		if runs, err := f.jobs.RunsForTask(ctx, tenant, other.ID); err != nil || runs == nil || len(runs) != 0 {
			t.Errorf("a task without jobs = %v, %v", runs, err)
		}
		if _, err := f.jobs.RunsForTask(ctx, "tenant_other", task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("another tenant: %v", err)
		}
	})
}

func TestOutboxService(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		woken := 0
		svc := NewOutboxService(f.b.Outbox, func() time.Time { return t0.Add(time.Hour) }, func() { woken++ })
		page, err := svc.List(ctx, repository.OutboxFilter{TenantID: tenant})
		if err != nil || len(page.Entries) != 1 || page.Entries[0].AggregateID != task.ID { // task.created to bob
			t.Fatalf("List = %+v, %v", page, err)
		}
		if _, err := svc.List(ctx, repository.OutboxFilter{TenantID: tenant, Status: "lost"}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("unknown status: %v", err)
		}
		claimed, err := f.b.Outbox.ClaimBatch(ctx, t0.Add(time.Minute), 10)
		if err != nil || len(claimed) == 0 {
			t.Fatalf("claim: %v", err)
		}
		row := claimed[0]
		if err := f.b.Outbox.MarkDeadLetter(ctx, row.ID, *row.ClaimedAt, 7, "400"); err != nil {
			t.Fatal(err)
		}
		dead, err := svc.List(ctx, repository.OutboxFilter{TenantID: tenant, Status: domain.OutboxDeadLetter})
		if err != nil || len(dead.Entries) != 1 {
			t.Fatalf("dead letters = %+v, %v", dead, err)
		}
		got, err := svc.Replay(ctx, tenant, row.ID)
		if err != nil || got.Status != domain.OutboxPending || !got.NextAttemptAt.Equal(t0.Add(time.Hour)) || woken != 1 {
			t.Fatalf("Replay = %+v, %v (woken %d)", got, err, woken)
		}
		if _, err := svc.Replay(ctx, tenant, row.ID); !errors.Is(err, domain.ErrNotDeadLetter) || woken != 1 {
			t.Errorf("replay of a pending row: %v (woken %d)", err, woken)
		}
		// Without a wake function or a clock.
		if err := f.b.Outbox.MarkDeadLetter(ctx, row.ID, *row.ClaimedAt, 7, "400"); err != nil {
			t.Fatal(err)
		}
		if _, err := NewOutboxService(f.b.Outbox, nil, nil).Replay(ctx, tenant, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("unknown row: %v", err)
		}
	})
}
