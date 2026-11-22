package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
)

// recordingSender records feedback and can fail.
type recordingSender struct {
	mu   sync.Mutex
	sent []agentpulse.Feedback
	err  error
}

func (r *recordingSender) RecordFeedback(_ context.Context, fb agentpulse.Feedback) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, fb)
	return nil
}

func (r *recordingSender) signals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, fb := range r.sent {
		out = append(out, fb.Signal)
	}
	return out
}

func TestWebhookDecisions(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		sender := &recordingSender{}
		hooks := NewWebhookService(f.b.Tx, f.b.Deliveries, f.svc, NewFeedbackService(f.b.Tasks, sender, nil), quiet, nil).
			WithAgentJobs(jf.jobs)
		task := f.create(t, nil) // assignee bob
		job := jf.proposal(t, task, "from Slack")
		cb := Callback{Key: "sig-1", Action: ActionApprove, TenantID: tenant, UserID: bob.ID, JobID: job.ID, NotificationID: "n1"}

		if dup, err := hooks.Handle(ctx, cb); err != nil || dup {
			t.Fatalf("approve = %v, %v", dup, err)
		}
		if got := f.get(t, task.ID); got.Description != "from Slack" {
			t.Errorf("task = %q", got.Description)
		}
		// A repeat, with a new key: nothing changes and no second signal.
		cb.Key = "sig-2"
		if dup, err := hooks.Handle(ctx, cb); err != nil || !dup {
			t.Errorf("repeat = %v, %v", dup, err)
		}
		if s := sender.signals(); len(s) != 1 || s[0] != SignalActed {
			t.Errorf("signals = %v", s)
		}

		// Reject another proposal while AgentPulse is down: the decision
		// stands, the failed feedback is only logged.
		second := jf.proposal(t, task, "no")
		sender.err = errors.New("agentpulse down")
		if dup, err := hooks.Handle(ctx, Callback{Key: "sig-3", Action: ActionReject, TenantID: tenant, UserID: bob.ID, JobID: second.ID}); err != nil || dup {
			t.Errorf("reject = %v, %v", dup, err)
		}
		if got := jf.job(t, second.ID); got.Status != domain.JobDismissed {
			t.Errorf("rejected job = %s", got.Status)
		}

		// Bad callbacks.
		for name, bad := range map[string]Callback{
			"no job":  {Key: "k", Action: ActionApprove, TenantID: tenant, UserID: bob.ID},
			"no user": {Key: "k", Action: ActionReject, TenantID: tenant, JobID: job.ID},
		} {
			if _, err := hooks.Handle(ctx, bad); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("%s: %v", name, err)
			}
		}
		without := NewWebhookService(f.b.Tx, f.b.Deliveries, f.svc, NewFeedbackService(f.b.Tasks, sender, nil), quiet, nil)
		if _, err := without.Handle(ctx, Callback{Key: "k", Action: ActionApprove, TenantID: tenant, UserID: bob.ID, JobID: job.ID}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("approvals not wired: %v", err)
		}
		if _, err := hooks.Handle(ctx, Callback{Key: "k", Action: ActionApprove, TenantID: tenant, UserID: "usr_eve", JobID: job.ID}); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("stranger: %v", err)
		}
	})
}

func TestJobWithoutATask(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		job, _ := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, AgentType: domain.AgentDigest,
			DedupKey: "digest:" + uuid.NewString()}, f.now)
		if _, err := f.b.AgentJobs.Insert(ctx, job); err != nil {
			t.Fatal(err)
		}
		prev := job.UpdatedAt
		job.Status, job.ProposedActions = domain.JobAwaitingApproval, json.RawMessage(`[]`)
		if err := f.b.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
			t.Fatal(err)
		}
		// A job about no task and no person is the admins': to a member it
		// does not exist (owner decision, 2026-11-23).
		if _, _, err := jf.jobs.Apply(ctx, tenant, job.ID, assignee); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("member on a task-less job: %v", err)
		}
		got, applied, err := jf.jobs.Apply(ctx, tenant, job.ID, admin)
		if err != nil || !applied || got.Status != domain.JobCompleted {
			t.Errorf("admin = %+v, %v, %v", got, applied, err)
		}
	})
}

// conflictingApplier fails as if the task moved while proposals applied.
type conflictingApplier struct{}

func (conflictingApplier) ApplyProposals(context.Context, *domain.AgentJob) error {
	return domain.ErrVersionConflict
}

func TestConflictWhileApplyingIsStale(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		task := f.create(t, nil)
		job := jf.proposal(t, task, "x")
		svc := NewAgentJobService(f.b.Tx, f.b.AgentJobs, f.b.Tasks, conflictingApplier{}, nil, nil)
		if _, _, err := svc.Apply(ctx, tenant, job.ID, assignee); !errors.Is(err, ErrStaleProposal) {
			t.Fatalf("Apply = %v", err)
		}
		if got := jf.job(t, job.ID); got.Status != domain.JobFailed || got.ErrorMessage != StaleVersion {
			t.Errorf("job = %s %q", got.Status, got.ErrorMessage)
		}
	})
}
