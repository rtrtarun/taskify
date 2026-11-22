package service

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// TestLearningSubmit is M11-03's write path: a suggestion's record and its
// outbox row commit together, a job submits a pattern once, and an
// adjustment outside -20 to 20 writes nothing.
func TestLearningSubmit(t *testing.T) {
	for _, factory := range repotest.Factories(t) {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			b := factory.New(t)
			now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
			var wakes atomic.Int32
			svc := NewLearningService(b.Tx, b.Learning, b.Outbox, 4, func() time.Time { return now }, func() { wakes.Add(1) })
			job, err := domain.NewLearningJob(domain.LearningTenant{TenantID: tenant, WorkspaceID: "ws_core"}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.AgentJobs.Insert(ctx, job); err != nil {
				t.Fatal(err)
			}
			docs := "docs"
			sg := domain.LearningSuggestion{TenantID: tenant, JobID: job.ID,
				Pattern:            domain.EventPattern{EventType: "task.comment_added", Label: &docs},
				ProposedAdjustment: -10, SignalCount: 16, Rationale: "16 of 20 dismissed or muted."}

			created, err := svc.Submit(ctx, sg)
			if err != nil || !created {
				t.Fatalf("Submit = %v, %v", created, err)
			}
			rows, err := b.Outbox.ListForAggregate(ctx, job.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("outbox = %v, %v", rows, err)
			}
			want := `{"tenantId":"tenant_acme","eventPattern":{"eventType":"task.comment_added","label":"docs"},` +
				`"currentAdjustment":0,"proposedAdjustment":-10,"signalCount":16,"rationale":"16 of 20 dismissed or muted."}`
			if r := rows[0]; r.EventType != domain.EventLearningSuggestion || r.Status != domain.OutboxPending || r.MaxAttempts != 4 ||
				!sameJSON(t, r.Payload, want) {
				t.Errorf("row = %s %s %d %s", r.EventType, r.Status, r.MaxAttempts, r.Payload)
			}
			if got, _ := b.Learning.PatternsSince(ctx, tenant, now.Add(-time.Hour)); len(got) != 1 || *got[0].Label != "docs" {
				t.Errorf("recorded patterns = %v", got)
			}

			// The same job and pattern again: nothing more is written.
			again := sg
			again.ProposedAdjustment = -15
			if created, err := svc.Submit(ctx, again); err != nil || created {
				t.Errorf("repeat = %v, %v", created, err)
			}
			if rows, _ := b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 1 {
				t.Errorf("outbox rows after a repeat = %d", len(rows))
			}

			// Out of range, or no change: refused, nothing written.
			for _, bad := range []int{-21, 21} {
				out := sg
				out.Pattern = domain.EventPattern{EventType: "task.updated"}
				out.ProposedAdjustment = bad
				if _, err := svc.Submit(ctx, out); !errors.Is(err, domain.ErrValidation) {
					t.Errorf("adjustment %d: %v", bad, err)
				}
			}
			if got, _ := b.Learning.PatternsSince(ctx, tenant, now.Add(-time.Hour)); len(got) != 1 {
				t.Errorf("a refused suggestion was recorded: %v", got)
			}
			if rows, _ := b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 1 {
				t.Errorf("a refused suggestion was queued: %d rows", len(rows))
			}
			if wakes.Load() != 1 {
				t.Errorf("relay woken %d times, want 1", wakes.Load())
			}
		})
	}
}

// sameJSON compares JSON documents by value: PostgreSQL's JSONB does not
// keep key order.
func sameJSON(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(a, b)
}
