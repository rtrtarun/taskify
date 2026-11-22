package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// hookedLLM runs before ahead of every model call.
type hookedLLM struct {
	next   llm.Client
	once   sync.Once
	before func()
}

func (h *hookedLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	h.once.Do(h.before)
	return h.next.Complete(ctx, req)
}

// TestBriefNamingADeletedTaskIsGatheredAgain is the runtime side of AP-08: a
// task permanently deleted while the model writes the brief is not stored
// in it. The run is redone like a version conflict, and the next run's brief
// leaves the task out.
func TestBriefNamingADeletedTaskIsGatheredAgain(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		mk := func(title string) *domain.Task {
			t.Helper()
			task, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: title,
				AssigneeID: "usr_bob"})
			if err != nil {
				t.Fatal(err)
			}
			return task
		}
		doomed := mk("Acquire Initech quietly")
		mk("Water the plants")
		job := f.queueBrief(t, "usr_bob", f.briefAt())

		hook := &hookedLLM{next: f.mock, before: func() {
			if err := f.svc.DeleteTask(ctx, "tenant_acme", doomed.ID); err != nil {
				t.Error(err)
			}
		}}
		f.mock.Then(llm.Text("Two things: the acquisition and the plants."))
		r, err := New(Deps{Tx: b.Tx, Outbox: b.Outbox, Jobs: b.AgentJobs, Tasks: b.Tasks, Workspaces: b.Workspaces, LLM: hook, Tools: f.reg},
			Config{Logger: quiet, Now: f.clock.now, PublicBaseURL: "https://taskify.test"}, f.digest())
		if err != nil {
			t.Fatal(err)
		}
		if n, err := r.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}
		got, res := f.result(t, job.ID)
		if got.Status != domain.JobPending || got.RetryAfter == nil || got.Attempts != 0 || got.ConflictCount != 1 || res.Report != nil {
			t.Fatalf("after the delete: %s, retry %v, attempts %d, conflicts %d, report %v",
				got.Status, got.RetryAfter, got.Attempts, got.ConflictCount, res.Report)
		}
		if rows, _ := b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 0 {
			t.Fatalf("a stale brief was sent: %d rows", len(rows))
		}

		f.clock.advance(time.Minute) // past the conflict delay
		f.mock.Then(llm.Text("One thing: the plants."))
		if n, err := f.digestRuntime(t).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("second RunOnce = %d, %v", n, err)
		}
		got, res = f.result(t, job.ID)
		if got.Status != domain.JobCompleted || res.Report == nil {
			t.Fatalf("second run: %s, %s", got.Status, got.ExecutionResult)
		}
		if names := titles(res.Report.TopItems); !slices.Equal(names, []string{"Water the plants"}) {
			t.Errorf("top items = %v", names)
		}
		if rows, _ := b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 1 {
			t.Errorf("brief rows = %d, want 1", len(rows))
		}
	})
}

// TestBriefRacesPermanentDelete: a brief being written and a permanent
// delete of a task it names run at the same time, many times. Whatever the
// interleaving, no stored brief and no outbox row names the deleted task.
func TestBriefRacesPermanentDelete(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		r := f.digestRuntime(t)
		scrubbed := 0
		defer func() { t.Logf("%d of 12 briefs were stored first and scrubbed", scrubbed) }()
		for i := 0; i < 12; i++ {
			user := "usr_racer" + string(rune('a'+i))
			doomed, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws",
				Title: "Secret " + user, AssigneeID: user})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws",
				Title: "Public " + user, AssigneeID: user}); err != nil {
				t.Fatal(err)
			}
			job := f.queueBrief(t, user, f.briefAt())
			for range 3 {
				f.mock.Then(llm.Text("Your brief."))
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(i%4) * time.Millisecond)
				if err := f.svc.DeleteTask(ctx, "tenant_acme", doomed.ID); err != nil {
					t.Error(err)
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := r.RunOnce(ctx); err != nil {
					t.Error(err)
				}
			}()
			wg.Wait()
			// The brief may have been stored first and then scrubbed with the
			// task: the job is gone, and so are its outbox rows.
			if _, err := b.AgentJobs.Get(ctx, "tenant_acme", job.ID); errors.Is(err, domain.ErrNotFound) {
				if rows, _ := b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 0 {
					t.Errorf("round %d: a scrubbed brief kept %d outbox rows", i, len(rows))
				}
				scrubbed++
				continue
			}
			for tries := 0; f.job(t, job.ID).Status == domain.JobPending && tries < 3; tries++ {
				f.clock.advance(time.Minute)
				if _, err := r.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			got, res := f.result(t, job.ID)
			if got.Status != domain.JobCompleted || res.Report == nil {
				t.Fatalf("round %d: job %s, %s", i, got.Status, got.ExecutionResult)
			}
			if slices.Contains(res.Report.TaskIDs(), doomed.ID) {
				t.Errorf("round %d: the stored brief names the deleted task", i)
			}
			rows, _ := b.Outbox.ListForAggregate(ctx, job.ID)
			for _, row := range rows {
				if strings.Contains(string(row.Payload), doomed.ID.String()) {
					t.Errorf("round %d: an outbox row names the deleted task", i)
				}
			}
		}
	})
}
