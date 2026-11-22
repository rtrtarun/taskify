package agent

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
)

func (f *fixture) digest() Digest {
	return Digest{Tasks: f.b.Tasks, Jobs: f.b.AgentJobs, Workspaces: f.b.Workspaces, Now: f.clock.now}
}

// digestRuntime runs the real digest agent, which needs no tools.
func (f *fixture) digestRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(Deps{Tx: f.b.Tx, Outbox: f.b.Outbox, Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg},
		Config{Logger: quiet, Now: f.clock.now, PublicBaseURL: "https://taskify.test", Workers: 4}, f.digest())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// briefAt is today's brief time by the fixture's clock.
func (f *fixture) briefAt() time.Time {
	now := f.clock.now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 8, 0, 0, 0, time.UTC)
}

// queueBrief queues user's brief for the day of at, naming workspace ws.
func (f *fixture) queueBrief(t *testing.T, user string, at time.Time) *domain.AgentJob {
	t.Helper()
	job, err := domain.NewBriefJob(domain.BriefRecipient{TenantID: "tenant_acme", UserID: user, WorkspaceID: "ws"}, at, f.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.b.AgentJobs.Insert(ctx, job); err != nil || !ok {
		t.Fatalf("insert brief: %v %v", ok, err)
	}
	return &job
}

// workspaceWith creates a workspace in tenant_acme with the given agent
// settings.
func (f *fixture) workspaceWith(t *testing.T, id string, p domain.AgentSettingsPatch) {
	t.Helper()
	w, _ := domain.NewWorkspace("tenant_acme", id, id, f.clock.now())
	if err := w.UpdateAgentSettings(p, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Workspaces.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
}

// propose leaves a decomposer job on task awaiting approval.
func (f *fixture) propose(t *testing.T, task *domain.Task) *domain.AgentJob {
	t.Helper()
	j, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: "tenant_acme", TaskID: &task.ID, AgentType: domain.AgentDecomposer,
		DedupKey: domain.RequestJobDedupKey(domain.AgentDecomposer, task.ID, uuid.New()), BaseTaskVersion: &task.Version}, f.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.AgentJobs.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.b.AgentJobs.Claim(ctx, f.clock.now(), 1, []string{domain.AgentDecomposer})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	job := claimed[0]
	prev := job.UpdatedAt
	job.ProposedActions = json.RawMessage(`[{"tool": "CreateSubtaskDAG", "arguments": {}}]`)
	if err := job.TransitionTo(domain.JobAwaitingApproval, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.b.AgentJobs.Update(ctx, job, domain.JobRunning, prev); err != nil {
		t.Fatal(err)
	}
	return job
}

// briefRows returns the outbox notifications a brief job wrote.
func (f *fixture) briefRows(t *testing.T, job *domain.AgentJob) []domain.Notification {
	t.Helper()
	entries, err := f.b.Outbox.ListForAggregate(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := []domain.Notification{}
	for _, e := range entries {
		var n domain.Notification
		if err := json.Unmarshal(e.Payload, &n); err != nil {
			t.Fatal(err)
		}
		if e.EventType != domain.EventDigestMorningBrief || e.TenantID != "tenant_acme" {
			t.Errorf("outbox row = %+v", e)
		}
		out = append(out, n)
	}
	return out
}

type storedResult struct {
	Turns      int                  `json:"turns"`
	Stopped    string               `json:"stopped"`
	SkipReason string               `json:"skip_reason"`
	Report     *domain.MorningBrief `json:"report"`
}

func (f *fixture) result(t *testing.T, id uuid.UUID) (*domain.AgentJob, storedResult) {
	t.Helper()
	job := f.job(t, id)
	var res storedResult
	if err := json.Unmarshal(job.ExecutionResult, &res); err != nil {
		t.Fatalf("execution_result %s: %v", job.ExecutionResult, err)
	}
	return job, res
}

func titles(items []domain.BriefTask) []string {
	out := []string{}
	for _, i := range items {
		out = append(out, i.Title)
	}
	return out
}

// TestDigestWritesBrief: the brief gathers the top three items, pending
// approvals, and newly unblocked tasks; the model writes only the text,
// with no tools; the brief is kept in the job and sent once on the digest
// channel (M10-02).
func TestDigestWritesBrief(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		f.workspaceWith(t, "ws_quiet", domain.AgentSettingsPatch{AgentsEnabled: ptr(true), DisabledAgents: &[]string{domain.AgentDigest}})
		bob := domain.User("usr_bob")
		mk := func(title string, p domain.Priority, due *time.Time) *domain.Task {
			t.Helper()
			task, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: title,
				Priority: p, AssigneeID: bob.ID, DueAt: due})
			if err != nil {
				t.Fatal(err)
			}
			return task
		}
		// Overdue at the brief time, whatever the time of day the test runs.
		past := f.briefAt().Add(-time.Hour)
		mk("Docs", domain.PriorityMedium, nil)
		review := mk("Review the design", domain.PriorityHigh, nil)
		outage := mk("Fix the outage", domain.PriorityCritical, nil)
		f.setStatus(t, bob, outage.ID, domain.StatusInProgress)
		mk("Send the invoice", domain.PriorityLow, &past) // overdue
		unblocked := mk("Load test", domain.PriorityLow, nil)
		f.setStatus(t, alice, unblocked.ID, domain.StatusBlocked)
		f.setStatus(t, alice, unblocked.ID, domain.StatusTodo)
		done := mk("Shipped already", domain.PriorityCritical, nil)
		f.setStatus(t, bob, done.ID, domain.StatusCompleted)
		quiet, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws_quiet",
			Title: "Hidden from the digest", Priority: domain.PriorityCritical, AssigneeID: bob.ID})
		if err != nil {
			t.Fatal(err)
		}
		approval := f.propose(t, review)
		f.propose(t, quiet) // in a workspace with the digest disabled

		job := f.queueBrief(t, bob.ID, f.briefAt())
		text := "Start with the overdue invoice, then the outage. One proposal waits on you, and the load test can start."
		f.mock.Then(llm.Text("  " + text + "\n"))
		if n, err := f.digestRuntime(t).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}

		got, res := f.result(t, job.ID)
		if got.Status != domain.JobCompleted || res.Stopped != stoppedFinal || res.Turns != 1 || res.Report == nil {
			t.Fatalf("job = %s (%s), result %s", got.Status, got.ErrorMessage, got.ExecutionResult)
		}
		r := res.Report
		if want := []string{"Send the invoice", "Fix the outage", "Review the design"}; !slices.Equal(titles(r.TopItems), want) {
			t.Errorf("top items = %v, want %v", titles(r.TopItems), want)
		}
		if !r.TopItems[0].Overdue || r.OpenTasks != 5 || r.Text != text || r.UserID != bob.ID ||
			r.Date != f.briefAt().Format(time.DateOnly) {
			t.Errorf("report = %+v", r)
		}
		if r.PendingApprovalsTotal != 1 || r.PendingApprovals[0].JobID != approval.ID || r.PendingApprovals[0].TaskTitle != review.Title ||
			!slices.Equal(r.PendingApprovals[0].Tools, []string{"CreateSubtaskDAG"}) {
			t.Errorf("approvals = %+v", r.PendingApprovals)
		}
		if !slices.Equal(titles(r.UnblockedTasks), []string{"Load test"}) || r.UnblockedTotal != 1 {
			t.Errorf("unblocked = %v", titles(r.UnblockedTasks))
		}

		// The model saw the gathered facts, in rank order, and had no tools.
		reqs := f.mock.Requests()
		if len(reqs) != 1 || len(reqs[0].Tools) != 0 {
			t.Fatalf("model requests = %d, tools %v", len(reqs), reqs)
		}
		facts := reqs[0].Messages[1].Content
		first, second := strings.Index(facts, "Send the invoice"), strings.Index(facts, "Fix the outage")
		if first < 0 || second < first || !strings.Contains(facts, "pending_approvals") || !strings.Contains(facts, "Load test") {
			t.Errorf("facts = %s", facts)
		}
		for _, hidden := range []string{"Shipped already", "Hidden from the digest"} {
			if strings.Contains(facts, hidden) {
				t.Errorf("the facts include %q", hidden)
			}
		}

		// Sent once, on the digest channel, with the text as digestSummary.
		rows := f.briefRows(t, job)
		if len(rows) != 1 {
			t.Fatalf("brief notifications = %d, want 1", len(rows))
		}
		n := rows[0]
		if n.UserID != bob.ID || !slices.Equal(n.Channels, []string{domain.ChannelDigest}) || n.Payload["digestSummary"] != text ||
			n.Payload["bypassDigest"] != nil || n.IdempotencyKey != "taskify:tenant_acme:"+job.ID.String()+":digest.morning_brief" {
			t.Errorf("notification = %+v", n)
		}
		if outcomes := f.outcomes(t, job.ID); !slices.Equal(outcomes, []string{OutcomeFinal}) {
			t.Errorf("run history = %v", outcomes)
		}

		// GET /me/brief (M10-03) reads back what the run stored.
		jobs := service.NewAgentJobService(b.Tx, b.AgentJobs, b.Tasks, f.reg, nil, f.clock.now)
		latest, err := jobs.LatestBrief(ctx, "tenant_acme", bob.ID)
		if err != nil || latest.JobID != job.ID || latest.Text != text || len(latest.TopItems) != 3 ||
			latest.PendingApprovalsTotal != 1 || latest.UnblockedTotal != 1 {
			t.Errorf("LatestBrief = %+v, %v", latest, err)
		}
		if _, err := jobs.LatestBrief(ctx, "tenant_acme", "usr_alice"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("alice has no brief: %v", err)
		}
	})
}

// TestDigestSkipsEmptyBrief is the M10 exit criterion "the brief for a user
// with no tasks is skipped, not sent empty": the job completes as skipped,
// the model is not called, and nothing is stored or sent.
func TestDigestSkipsEmptyBrief(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		f.workspaceWith(t, "ws_quiet", domain.AgentSettingsPatch{AgentsEnabled: ptr(true), DisabledAgents: &[]string{domain.AgentDigest}})
		dave := domain.User("usr_dave")
		finished := f.owned(t, "usr_alice", dave.ID, "Finished")
		f.setStatus(t, dave, finished.ID, domain.StatusCompleted)
		waiting := f.owned(t, "usr_alice", dave.ID, "Waiting")
		f.setStatus(t, alice, waiting.ID, domain.StatusBlocked) // blocked work is not actionable
		if _, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws_quiet",
			Title: "Digest off here", AssigneeID: "usr_carol"}); err != nil {
			t.Fatal(err)
		}

		jobs := []*domain.AgentJob{
			f.queueBrief(t, "usr_nobody", f.briefAt()), // no tasks at all
			f.queueBrief(t, dave.ID, f.briefAt()),      // only finished or blocked work
			f.queueBrief(t, "usr_carol", f.briefAt()),  // work only where the digest is off
		}
		if n, err := f.digestRuntime(t).RunOnce(ctx); err != nil || n != len(jobs) {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}
		for _, j := range jobs {
			got, res := f.result(t, j.ID)
			if got.Status != domain.JobCompleted || res.Stopped != stoppedSkipped || res.SkipReason != "nothing to report" ||
				res.Turns != 0 || res.Report != nil {
				t.Errorf("job %s = %s, result %s", j.DedupKey, got.Status, got.ExecutionResult)
			}
			if rows := f.briefRows(t, j); len(rows) != 0 {
				t.Errorf("job %s sent %v", j.DedupKey, rows)
			}
			if outcomes := f.outcomes(t, j.ID); len(outcomes) != 0 {
				t.Errorf("job %s has run history %v", j.DedupKey, outcomes)
			}
		}
		if reqs := f.mock.Requests(); len(reqs) != 0 {
			t.Errorf("the model was called %d times", len(reqs))
		}
	})
}

// TestDigestOutOfDateBrief: a brief for a day that has passed, as when the
// runtime was off, is skipped rather than sent late.
func TestDigestOutOfDateBrief(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		f.owned(t, "usr_alice", "usr_bob", "Open work")
		job := f.queueBrief(t, "usr_bob", f.briefAt().Add(-24*time.Hour))
		if n, err := f.digestRuntime(t).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}
		got, res := f.result(t, job.ID)
		if got.Status != domain.JobCompleted || res.Stopped != stoppedSkipped || !strings.HasPrefix(res.SkipReason, "out of date") {
			t.Errorf("job = %s, result %s", got.Status, got.ExecutionResult)
		}
		if len(f.mock.Requests()) != 0 || len(f.briefRows(t, job)) != 0 {
			t.Error("an out-of-date brief was written or sent")
		}
	})
}

// TestDigestRetriesWithoutText: a run in which the model writes nothing
// stores and sends nothing and is retried; the retry sends the brief once.
func TestDigestRetriesWithoutText(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		f.owned(t, "usr_alice", "usr_bob", "Open work")
		job := f.queueBrief(t, "usr_bob", f.briefAt())
		f.mock.Then(llm.Text("   "))
		if _, err := f.digestRuntime(t).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, res := f.result(t, job.ID)
		if got.Status != domain.JobPending || got.Attempts != 1 || res.Report != nil || len(f.briefRows(t, job)) != 0 {
			t.Fatalf("after an empty answer: %s, %d attempts, result %s", got.Status, got.Attempts, got.ExecutionResult)
		}

		f.clock.advance(time.Minute) // past the retry backoff
		f.mock.Then(llm.Text("Open work is your only task today."))
		if n, err := f.digestRuntime(t).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("retry RunOnce = %d, %v", n, err)
		}
		if got, res := f.result(t, job.ID); got.Status != domain.JobCompleted || res.Report == nil {
			t.Fatalf("after the retry: %s, result %s", got.Status, got.ExecutionResult)
		}
		if rows := f.briefRows(t, job); len(rows) != 1 {
			t.Errorf("brief notifications = %d, want 1", len(rows))
		}
	})
}
