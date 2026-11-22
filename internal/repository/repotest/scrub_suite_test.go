package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// storeBriefNaming stores a completed brief job for user on day whose
// report lists tasks in the given part: top_items, pending_approvals, or
// unblocked_tasks.
func storeBriefNaming(t *testing.T, b Backend, tenantID, user string, day time.Time, part string, tasks ...uuid.UUID) domain.AgentJob {
	t.Helper()
	brief := domain.MorningBrief{Date: day.Format(time.DateOnly), UserID: user, Text: "Brief",
		TopItems: []domain.BriefTask{}, PendingApprovals: []domain.BriefApproval{}, UnblockedTasks: []domain.BriefTask{}}
	for _, id := range tasks {
		switch part {
		case "top_items":
			brief.TopItems = append(brief.TopItems, domain.BriefTask{ID: id, Title: "Named"})
		case "pending_approvals":
			brief.PendingApprovals = append(brief.PendingApprovals, domain.BriefApproval{JobID: uuid.New(), TaskID: id, TaskTitle: "Named"})
		default:
			brief.UnblockedTasks = append(brief.UnblockedTasks, domain.BriefTask{ID: id, Title: "Named"})
		}
	}
	report, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	job, err := domain.NewBriefJob(domain.BriefRecipient{TenantID: tenantID, UserID: user, WorkspaceID: ws}, day, day)
	if err != nil {
		t.Fatal(err)
	}
	insertJob(t, b, job)
	prev := job.UpdatedAt
	job.Status, job.UpdatedAt = domain.JobCompleted, day.Add(time.Minute)
	job.ExecutionResult = json.RawMessage(`{"turns": 1, "stopped": "final", "report": ` + string(report) + `}`)
	if err := b.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
		t.Fatal(err)
	}
	return job
}

// TestDeleteBriefsMentioning is the repository side of AP-08.
func TestDeleteBriefsMentioning(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		day := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
		named, other := uuid.New(), uuid.New()
		top := storeBriefNaming(t, b, tenant, "usr_bob", day, "top_items", other, named)
		approval := storeBriefNaming(t, b, tenant, "usr_carol", day, "pending_approvals", named)
		unblocked := storeBriefNaming(t, b, tenant, "usr_dave", day, "unblocked_tasks", named)
		kept := storeBriefNaming(t, b, tenant, "usr_bob", day.Add(24*time.Hour), "top_items", other)
		theirs := storeBriefNaming(t, b, "tenant_beta", "usr_bob", day, "top_items", named)
		skipped := storeBrief(t, b, tenant, "usr_erin", day, false)
		if err := b.AgentJobs.InsertRun(ctx, domain.AgentRun{ID: uuid.New(), JobID: top.ID, Turn: 1, Model: "m", Outcome: "final", CreatedAt: day}); err != nil {
			t.Fatal(err)
		}

		gone, err := b.AgentJobs.DeleteBriefsMentioning(ctx, tenant, named)
		if err != nil {
			t.Fatal(err)
		}
		want := []uuid.UUID{top.ID, approval.ID, unblocked.ID}
		slices.SortFunc(gone, func(a, b uuid.UUID) int { return compareUUID(a, b) })
		slices.SortFunc(want, func(a, b uuid.UUID) int { return compareUUID(a, b) })
		if !slices.Equal(gone, want) {
			t.Errorf("deleted %v, want %v", gone, want)
		}
		for _, j := range []domain.AgentJob{top, approval, unblocked} {
			if _, err := b.AgentJobs.Get(ctx, tenant, j.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("job %s survived: %v", j.ID, err)
			}
		}
		for _, j := range []domain.AgentJob{kept, skipped} {
			if _, err := b.AgentJobs.Get(ctx, tenant, j.ID); err != nil {
				t.Errorf("job %s was deleted: %v", j.ID, err)
			}
		}
		if _, err := b.AgentJobs.Get(ctx, "tenant_beta", theirs.ID); err != nil {
			t.Errorf("another tenant's brief was deleted: %v", err)
		}
		if runs, _ := b.AgentJobs.ListRuns(ctx, top.ID); len(runs) != 0 {
			t.Errorf("%d runs survived their job", len(runs))
		}
		if again, err := b.AgentJobs.DeleteBriefsMentioning(ctx, tenant, named); err != nil || len(again) != 0 {
			t.Errorf("second delete = %v, %v", again, err)
		}
		// The newest brief that remains is served instead.
		if got, err := b.AgentJobs.LatestBrief(ctx, tenant, "usr_bob"); err != nil || got.ID != kept.ID {
			t.Errorf("bob's latest = %v, %v", got, err)
		}
		if _, err := b.AgentJobs.LatestBrief(ctx, tenant, "usr_carol"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("carol's latest: %v", err)
		}
	})
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return 0
}

func TestTaskExistingAndBriefLock(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		if err := b.Tasks.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		missing := uuid.New()
		found, err := b.Tasks.Existing(ctx, tenant, []uuid.UUID{missing, task.ID, task.ID})
		if err != nil || !slices.Equal(found, []uuid.UUID{task.ID}) {
			t.Errorf("Existing = %v, %v", found, err)
		}
		if found, _ := b.Tasks.Existing(ctx, "tenant_beta", []uuid.UUID{task.ID}); len(found) != 0 {
			t.Errorf("another tenant's task exists: %v", found)
		}
		if found, err := b.Tasks.Existing(ctx, tenant, nil); err != nil || len(found) != 0 {
			t.Errorf("no IDs = %v, %v", found, err)
		}
		if err := b.AgentJobs.LockBriefs(ctx, tenant, true); !errors.Is(err, repository.ErrTxRequired) {
			t.Errorf("LockBriefs outside a transaction: %v", err)
		}
		for _, exclusive := range []bool{false, true} {
			if err := b.Tx.WithTx(ctx, func(ctx context.Context) error { return b.AgentJobs.LockBriefs(ctx, tenant, exclusive) }); err != nil {
				t.Errorf("LockBriefs(%v): %v", exclusive, err)
			}
		}
	})
}

// TestBriefLockExcludes: on PostgreSQL a permanent delete's exclusive brief
// lock holds back a brief being stored in the tenant until it commits, and
// leaves other tenants alone. (The memory store serialises every
// transaction.)
func TestBriefLockExcludes(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		if b.DB == nil {
			t.Skip("the memory store serialises transactions")
		}
		hold := make(chan struct{})
		held := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- b.Tx.WithTx(ctx, func(ctx context.Context) error {
				if err := b.AgentJobs.LockBriefs(ctx, tenant, true); err != nil {
					return err
				}
				close(held)
				<-hold
				return nil
			})
		}()
		<-held
		// Another tenant is not held back.
		if err := b.Tx.WithTx(ctx, func(ctx context.Context) error { return b.AgentJobs.LockBriefs(ctx, "tenant_beta", false) }); err != nil {
			t.Fatal(err)
		}
		got := make(chan time.Time, 1)
		go func() {
			_ = b.Tx.WithTx(ctx, func(ctx context.Context) error {
				err := b.AgentJobs.LockBriefs(ctx, tenant, false)
				got <- time.Now()
				return err
			})
		}()
		select {
		case <-got:
			t.Fatal("a shared lock was granted while the exclusive one was held")
		case <-time.After(200 * time.Millisecond):
		}
		released := time.Now()
		close(hold)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if at := <-got; at.Before(released) {
			t.Error("the shared lock was granted before the exclusive one was released")
		}
	})
}
