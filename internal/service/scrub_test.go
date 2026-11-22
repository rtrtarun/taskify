package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

// storeBrief stores a written brief for user that lists tasks among its top
// items, with its digest.morning_brief outbox row, and returns the job.
func (f *fixture) storeBrief(t *testing.T, user string, day time.Time, tasks ...*domain.Task) *domain.AgentJob {
	t.Helper()
	job, err := domain.NewBriefJob(domain.BriefRecipient{TenantID: tenant, UserID: user, WorkspaceID: "ws_core"}, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.AgentJobs.Insert(ctx, job); err != nil {
		t.Fatal(err)
	}
	brief := domain.MorningBrief{TenantID: tenant, JobID: job.ID, Date: day.Format(time.DateOnly), UserID: user,
		TopItems: []domain.BriefTask{}, PendingApprovals: []domain.BriefApproval{}, UnblockedTasks: []domain.BriefTask{}}
	var titles []string
	for _, task := range tasks {
		brief.TopItems = append(brief.TopItems, domain.BriefTask{ID: task.ID, Title: task.Title, WorkspaceID: task.WorkspaceID,
			Status: task.Status, Priority: task.Priority})
		titles = append(titles, task.Title)
	}
	brief.OpenTasks = len(tasks)
	brief.Text = "First: " + strings.Join(titles, ", ")
	report, _ := json.Marshal(brief)
	prev := job.UpdatedAt
	job.Status, job.UpdatedAt = domain.JobCompleted, day.Add(time.Minute)
	job.ExecutionResult = json.RawMessage(`{"turns": 1, "stopped": "final", "final_message": "x", "report": ` + string(report) + `}`)
	if err := f.b.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
		t.Fatal(err)
	}
	entries, err := domain.NewNotificationEntries([]domain.Notification{brief.Notification(domain.Links{API: "https://taskify.test"})}, job.ID, 5, day)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.Outbox.Insert(ctx, entries); err != nil {
		t.Fatal(err)
	}
	return &job
}

// TestDeleteTaskScrubsBriefs is AP-08: a permanent delete also deletes the
// stored briefs that name the task and their outbox rows, in the same
// transaction, and leaves other briefs alone.
func TestDeleteTaskScrubsBriefs(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		secret := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Acquire Initech quietly" })
		other := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Water the plants" })
		day := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

		older := f.storeBrief(t, bob.ID, day, other)
		sent := f.storeBrief(t, bob.ID, day.Add(24*time.Hour), secret, other)
		waiting := f.storeBrief(t, bob.ID, day.Add(48*time.Hour), other, secret)
		carols := f.storeBrief(t, "usr_carol", day.Add(48*time.Hour), other)
		// The relay already sent one brief and holds another.
		claimed, err := f.b.Outbox.ClaimBatch(ctx, time.Now().Add(time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range claimed {
			if e.AggregateID == sent.ID {
				if err := f.b.Outbox.MarkDispatched(ctx, e.ID, *e.ClaimedAt, day); err != nil {
					t.Fatal(err)
				}
			}
		}

		if err := f.svc.DeleteTask(ctx, tenant, secret.ID); err != nil {
			t.Fatal(err)
		}
		for _, j := range []*domain.AgentJob{sent, waiting} {
			if _, err := f.b.AgentJobs.Get(ctx, tenant, j.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("brief %s survived: %v", j.ID, err)
			}
			if rows, _ := f.b.Outbox.ListForAggregate(ctx, j.ID); len(rows) != 0 {
				t.Errorf("brief %s kept %d outbox rows", j.ID, len(rows))
			}
		}
		for _, j := range []*domain.AgentJob{older, carols} {
			if _, err := f.b.AgentJobs.Get(ctx, tenant, j.ID); err != nil {
				t.Errorf("brief %s was deleted: %v", j.ID, err)
			}
			if rows, _ := f.b.Outbox.ListForAggregate(ctx, j.ID); len(rows) != 1 {
				t.Errorf("brief %s has %d outbox rows, want 1", j.ID, len(rows))
			}
		}
		// Nothing stored names the task any more.
		for _, user := range []string{bob.ID, "usr_carol"} {
			job, err := f.b.AgentJobs.LatestBrief(ctx, tenant, user)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(job.ExecutionResult), secret.Title) || strings.Contains(string(job.ExecutionResult), secret.ID.String()) {
				t.Errorf("%s's latest brief still names the task", user)
			}
		}
		if got, _ := f.b.AgentJobs.LatestBrief(ctx, tenant, bob.ID); got.ID != older.ID {
			t.Errorf("bob's latest brief = %s, want the older one", got.ID)
		}
	})
}
