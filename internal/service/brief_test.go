package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

// completedBrief stores user's brief job for day as the digest agent
// completes it, with result as its execution_result.
func (f *jobsFixture) completedBrief(t *testing.T, tenantID, user string, day time.Time, result string) *domain.AgentJob {
	t.Helper()
	job, err := domain.NewBriefJob(domain.BriefRecipient{TenantID: tenantID, UserID: user, WorkspaceID: "ws_core"}, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.AgentJobs.Insert(ctx, job); err != nil {
		t.Fatal(err)
	}
	prev := job.UpdatedAt
	job.Status, job.UpdatedAt = domain.JobCompleted, day.Add(time.Minute)
	job.ExecutionResult = json.RawMessage(result)
	if err := f.b.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
		t.Fatal(err)
	}
	return &job
}

// writtenBrief is the execution_result of a brief the model wrote.
func writtenBrief(user, day, text string) string {
	report, _ := json.Marshal(domain.MorningBrief{Date: day, UserID: user, Text: text, OpenTasks: 2,
		TopItems: []domain.BriefTask{}, PendingApprovals: []domain.BriefApproval{}, UnblockedTasks: []domain.BriefTask{}})
	return `{"turns": 1, "stopped": "final", "report": ` + string(report) + `}`
}

// TestLatestBrief: a person reads their most recent written brief; a day
// with nothing to report leaves the previous one in place, and briefs are
// kept apart by tenant and person (G15).
func TestLatestBrief(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		day := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
		if _, err := jf.jobs.LatestBrief(ctx, tenant, bob.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("no brief yet: %v", err)
		}

		jf.completedBrief(t, tenant, bob.ID, day, writtenBrief(bob.ID, "2026-11-20", "Friday's brief"))
		saturday := jf.completedBrief(t, tenant, bob.ID, day.Add(24*time.Hour), writtenBrief(bob.ID, "2026-11-21", "Saturday's brief"))
		jf.completedBrief(t, tenant, bob.ID, day.Add(48*time.Hour), `{"turns": 0, "stopped": "skipped", "skip_reason": "nothing to report"}`)
		carols := jf.completedBrief(t, tenant, "usr_carol", day.Add(48*time.Hour), writtenBrief("usr_carol", "2026-11-22", "Carol's brief"))
		jf.completedBrief(t, "tenant_other", bob.ID, day.Add(48*time.Hour), writtenBrief(bob.ID, "2026-11-22", "Another tenant's bob"))

		got, err := jf.jobs.LatestBrief(ctx, tenant, bob.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.JobID != saturday.ID || got.MorningBrief.JobID != saturday.ID || got.TenantID != tenant ||
			!got.WrittenAt.Equal(day.Add(24*time.Hour+time.Minute)) || got.Text != "Saturday's brief" ||
			got.Date != "2026-11-21" || got.UserID != bob.ID || got.OpenTasks != 2 {
			t.Errorf("bob's brief = %+v", got)
		}
		if got, err := jf.jobs.LatestBrief(ctx, tenant, "usr_carol"); err != nil || got.JobID != carols.ID || got.Text != "Carol's brief" {
			t.Errorf("carol's brief = %+v, %v", got, err)
		}
		if _, err := jf.jobs.LatestBrief(ctx, tenant, "usr_dave"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("dave has no brief: %v", err)
		}

		// A stored report that cannot be read is an error, never an empty
		// brief.
		for i, result := range []string{`{"stopped": "final", "report": null}`, `{"stopped": "final", "report": "garbled"}`} {
			bad := jf.completedBrief(t, tenant, bob.ID, day.Add(time.Duration(72+24*i)*time.Hour), result)
			if _, err := jf.jobs.LatestBrief(ctx, tenant, bob.ID); err == nil || !strings.Contains(err.Error(), bad.ID.String()) {
				t.Errorf("unreadable report %s: error = %v", result, err)
			}
		}
	})
}
