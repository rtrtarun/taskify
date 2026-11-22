package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

type faultSuggestions struct {
	repository.LearningSuggestionRepository
	in *injector
}

func (f faultSuggestions) Insert(ctx context.Context, s domain.LearningSuggestion) (bool, error) {
	if err := f.in.hit(); err != nil {
		return false, err
	}
	return f.LearningSuggestionRepository.Insert(ctx, s)
}

// learningJob stores a learning job for the tenant and returns it.
func learningJob(t *testing.T, b repotest.Backend, now time.Time) domain.AgentJob {
	t.Helper()
	job, err := domain.NewLearningJob(domain.LearningTenant{TenantID: tenant, WorkspaceID: "ws_core"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AgentJobs.Insert(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job
}

// TestLearningSubmitFailuresWriteNothing (M11-03): the suggestion's record
// and its outbox row commit together, so a failure at either leaves
// neither, and the job may submit the pattern again.
func TestLearningSubmitFailuresWriteNothing(t *testing.T) {
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	for failAt := 1; ; failAt++ {
		b := repotest.Memory()
		job := learningJob(t, b, now)
		in := &injector{failAt: failAt}
		woken := false
		svc := NewLearningService(b.Tx, faultSuggestions{b.Learning, in}, faultOutbox{b.Outbox, in}, 4,
			func() time.Time { return now }, func() { woken = true })
		sg := domain.LearningSuggestion{TenantID: tenant, JobID: job.ID, Pattern: domain.EventPattern{EventType: "task.updated"},
			ProposedAdjustment: -5, SignalCount: 12, Rationale: "12 of 15 dismissed."}
		created, err := svc.Submit(ctx, sg)
		if in.calls < failAt {
			if err != nil || !created || !woken || failAt == 1 {
				t.Fatalf("clean run = %v, %v, woken %v, after %d calls", created, err, woken, in.calls)
			}
			return
		}
		if !errors.Is(err, errInjected) || created {
			t.Errorf("failing call %d: Submit = %v, %v; want the injected failure", failAt, created, err)
		}
		if got, _ := b.Learning.PatternsSince(ctx, tenant, now.Add(-time.Hour)); len(got) != 0 {
			t.Errorf("failing call %d recorded %v", failAt, got)
		}
		if rows, _ := b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 0 {
			t.Errorf("failing call %d queued %d rows", failAt, len(rows))
		}
		if woken {
			t.Errorf("failing call %d woke the relay", failAt)
		}
		// Nothing was recorded, so the same job may submit it again.
		if created, err := NewLearningService(b.Tx, b.Learning, b.Outbox, 4, nil, nil).Submit(ctx, sg); err != nil || !created {
			t.Errorf("failing call %d: resubmit = %v, %v", failAt, created, err)
		}
	}
}

// TestLearningServiceDefaults: with no clock, attempts, or wake, a
// submission is stamped now and queued with 10 attempts.
func TestLearningServiceDefaults(t *testing.T) {
	b := repotest.Memory()
	before := time.Now().UTC()
	job := learningJob(t, b, before)
	svc := NewLearningService(b.Tx, b.Learning, b.Outbox, 0, nil, nil)
	sg := domain.LearningSuggestion{TenantID: tenant, JobID: job.ID, Pattern: domain.EventPattern{EventType: "task.updated"},
		ProposedAdjustment: 5, SignalCount: 12, Rationale: "Acted on 12 of 15."}
	if created, err := svc.Submit(ctx, sg); err != nil || !created {
		t.Fatalf("Submit = %v, %v", created, err)
	}
	rows, err := b.Outbox.ListForAggregate(ctx, job.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if r := rows[0]; r.MaxAttempts != 10 || r.CreatedAt.Before(before.Truncate(time.Microsecond)) || r.CreatedAt.After(time.Now()) {
		t.Errorf("row = %d attempts, created %v (test began %v)", r.MaxAttempts, r.CreatedAt, before)
	}
	if got, _ := b.Learning.PatternsSince(ctx, tenant, before.Add(-time.Second)); len(got) != 1 {
		t.Errorf("patterns since the test began = %v", got)
	}
}
