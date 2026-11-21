package service

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/agenttest"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
)

// jobsFixture adds an AgentJobService to the task fixture.
type jobsFixture struct {
	*fixture
	jobs    *AgentJobService
	metrics *observability.Metrics
}

func newJobsFixture(t *testing.T, f *fixture) *jobsFixture {
	t.Helper()
	reg := tools.NewRegistry(f.b.Tasks)
	if err := reg.Register(agenttest.DescribeTool{Tasks: f.svc, Tier2: true}); err != nil {
		t.Fatal(err)
	}
	m := observability.NewMetrics(nil)
	return &jobsFixture{fixture: f, metrics: m,
		jobs: NewAgentJobService(f.b.Tx, f.b.AgentJobs, f.b.Tasks, reg, m, func() time.Time { return f.now })}
}

// proposal stores a job awaiting approval of Describe calls on task, as the
// runtime would, based on the task's current version.
func (f *jobsFixture) proposal(t *testing.T, task *domain.Task, descriptions ...string) *domain.AgentJob {
	t.Helper()
	cur := f.get(t, task.ID)
	job, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, TaskID: &cur.ID, AgentType: domain.AgentTriage,
		DedupKey: "test:" + uuid.NewString(), BaseTaskVersion: &cur.Version}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.AgentJobs.Insert(ctx, job); err != nil {
		t.Fatal(err)
	}
	var props []tools.Proposal
	for i, d := range descriptions {
		args, _ := json.Marshal(agenttest.DescribeArgs{TaskID: cur.ID, Description: d})
		props = append(props, tools.Proposal{Tool: agenttest.DescribeToolName, Arguments: args, CallID: "c" + strconv.Itoa(i)})
	}
	prev := job.UpdatedAt
	job.Status = domain.JobAwaitingApproval
	job.ProposedActions, _ = json.Marshal(props)
	if err := f.b.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
		t.Fatal(err)
	}
	return &job
}

func (f *jobsFixture) job(t *testing.T, id uuid.UUID) *domain.AgentJob {
	t.Helper()
	j, err := f.b.AgentJobs.Get(ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

var (
	assignee = Decider{UserID: bob.ID}
	stranger = Decider{UserID: "usr_eve"}
	admin    = Decider{UserID: "usr_admin", Admin: true}
)

func TestApplyAppliesOnce(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		f.enableAgents(t) // so a wrongly attributed change would queue triage
		task := f.create(t, nil)
		job := jf.proposal(t, task, "first", "second")
		count := func() int {
			all, err := f.b.AgentJobs.ListForTask(ctx, tenant, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			return len(all)
		}
		before := count() // the person's triage job and this proposal

		got, applied, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee)
		if err != nil || !applied || got.Status != domain.JobCompleted || got.DecidedBy != bob.ID || got.DecidedAt == nil {
			t.Fatalf("Apply = %+v, %v, %v", got, applied, err)
		}
		stored := f.get(t, task.ID)
		if stored.Description != "second" || stored.Version != task.Version+2 {
			t.Errorf("task = %q v%d", stored.Description, stored.Version)
		}
		// Applied as the agent: notifications go out, no agent work is queued.
		if after := count(); after != before {
			t.Errorf("applying queued agent jobs: %d -> %d", before, after)
		}
		entries, _ := f.b.Outbox.ListForAggregate(ctx, task.ID)
		last := entries[len(entries)-1]
		var n domain.Notification
		_ = json.Unmarshal(last.Payload, &n)
		if last.EventType != domain.EventTaskUpdated || n.Payload["actorKind"] != "agent" {
			t.Errorf("last event = %s by %v", last.EventType, n.Payload["actorKind"])
		}

		// The same decision again is a no-op; the opposite one is refused.
		if again, applied, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee); err != nil || applied || again.Status != domain.JobCompleted {
			t.Errorf("second Apply = %v, %v", applied, err)
		}
		if f.get(t, task.ID).Version != stored.Version {
			t.Error("a second Apply changed the task")
		}
		if _, _, err := jf.jobs.Dismiss(ctx, tenant, job.ID, assignee); !errors.Is(err, ErrAlreadyDecided) {
			t.Errorf("Dismiss after Apply = %v", err)
		}
		if body := scrapeMetrics(t, jf.metrics); !contains(body, `taskify_agent_proposals_total{agent_type="triage",decision="approved"} 2`) {
			t.Errorf("approved proposals not counted:\n%s", body)
		}
	})
}

func TestDismissChangesNothing(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		task := f.create(t, nil)
		job := jf.proposal(t, task, "never")
		got, dismissed, err := jf.jobs.Dismiss(ctx, tenant, job.ID, Decider{UserID: alice.ID}) // the creator
		if err != nil || !dismissed || got.Status != domain.JobDismissed || got.DecidedBy != alice.ID {
			t.Fatalf("Dismiss = %+v, %v, %v", got, dismissed, err)
		}
		if stored := f.get(t, task.ID); stored.Version != task.Version || stored.Description != "" {
			t.Errorf("task changed: %+v", stored)
		}
		if _, again, err := jf.jobs.Dismiss(ctx, tenant, job.ID, assignee); err != nil || again {
			t.Errorf("second Dismiss = %v, %v", again, err)
		}
		if _, _, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee); !errors.Is(err, ErrAlreadyDecided) {
			t.Errorf("Apply after Dismiss = %v", err)
		}
	})
}

func TestStaleProposalFailsSafely(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		task := f.create(t, nil)
		job := jf.proposal(t, task, "too late")
		// A person edits the task before anyone approves.
		renamed := f.edit(t, alice, task, domain.Patch{Title: ptr("Renamed")})

		got, applied, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee)
		if !errors.Is(err, ErrStaleProposal) || applied {
			t.Fatalf("Apply = %v, %v", applied, err)
		}
		stored := jf.job(t, job.ID)
		if stored.Status != domain.JobFailed || stored.ErrorMessage != StaleVersion || stored.DecidedBy != bob.ID {
			t.Errorf("job = %s %q by %q", stored.Status, stored.ErrorMessage, stored.DecidedBy)
		}
		if got == nil || got.Status != domain.JobFailed {
			t.Errorf("returned job = %+v", got)
		}
		if cur := f.get(t, task.ID); cur.Version != renamed.Version || cur.Description != "" {
			t.Errorf("a stale proposal changed the task: %+v", cur)
		}
		// Approving again gives the same answer; dismissing is refused.
		if _, _, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee); !errors.Is(err, ErrStaleProposal) {
			t.Errorf("second Apply = %v", err)
		}
		if _, _, err := jf.jobs.Dismiss(ctx, tenant, job.ID, assignee); !errors.Is(err, ErrAlreadyDecided) {
			t.Errorf("Dismiss of a stale job = %v", err)
		}
	})
}

func TestWhoMayDecide(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		task := f.create(t, nil)
		job := jf.proposal(t, task, "x")
		for name, call := range map[string]func() error{
			"apply":   func() error { _, _, err := jf.jobs.Apply(ctx, tenant, job.ID, stranger); return err },
			"dismiss": func() error { _, _, err := jf.jobs.Dismiss(ctx, tenant, job.ID, stranger); return err },
		} {
			if err := call(); !errors.Is(err, domain.ErrForbidden) {
				t.Errorf("%s by a stranger = %v", name, err)
			}
		}
		if jf.job(t, job.ID).Status != domain.JobAwaitingApproval {
			t.Error("a refused decision changed the job")
		}
		// Another tenant cannot see the job at all.
		if _, _, err := jf.jobs.Apply(ctx, "tenant_other", job.ID, admin); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("other tenant = %v", err)
		}
		// An admin may decide.
		if _, ok, err := jf.jobs.Dismiss(ctx, tenant, job.ID, admin); err != nil || !ok {
			t.Errorf("admin Dismiss = %v, %v", ok, err)
		}
	})
}

func TestDecisionBeforeTheJobAsks(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		task := f.create(t, nil)
		job, _ := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, TaskID: &task.ID, AgentType: domain.AgentTriage,
			DedupKey: "pending:" + uuid.NewString()}, f.now)
		if _, err := f.b.AgentJobs.Insert(ctx, job); err != nil {
			t.Fatal(err)
		}
		if _, _, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee); !errors.Is(err, ErrNotAwaitingApproval) {
			t.Errorf("Apply on a pending job = %v", err)
		}
		if got, err := jf.jobs.Get(ctx, tenant, job.ID); err != nil || got.ID != job.ID {
			t.Errorf("Get = %v, %v", got, err)
		}
	})
}

func TestFailedApplyIsAtomic(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		task := f.create(t, nil)
		other := f.create(t, nil)
		job := jf.proposal(t, task, "first")
		// A second proposal aimed outside the job's scope: rejected on apply.
		var props []tools.Proposal
		_ = json.Unmarshal(job.ProposedActions, &props)
		args, _ := json.Marshal(agenttest.DescribeArgs{TaskID: other.ID, Description: "elsewhere"})
		props = append(props, tools.Proposal{Tool: agenttest.DescribeToolName, Arguments: args})
		prev := job.UpdatedAt
		job.ProposedActions, _ = json.Marshal(props)
		job.UpdatedAt = prev.Add(time.Second)
		if err := f.b.AgentJobs.Update(ctx, job, domain.JobAwaitingApproval, prev); err != nil {
			t.Fatal(err)
		}

		_, applied, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee)
		if err == nil || applied || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Apply = %v, %v", applied, err)
		}
		if cur := f.get(t, task.ID); cur.Description != "" || cur.Version != task.Version {
			t.Errorf("the first proposal was not rolled back: %+v", cur)
		}
		if got := jf.job(t, job.ID); got.Status != domain.JobFailed || got.ErrorMessage == "" {
			t.Errorf("job = %s %q", got.Status, got.ErrorMessage)
		}
	})
}

func scrapeMetrics(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
