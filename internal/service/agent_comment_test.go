package service

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

var blockerAgent = domain.Agent(domain.AgentBlocker)

// TestAddAgentCommentOncePerJob: an agent's comment is stored with its run
// in the task's agent_metadata and sends task.comment_added, without moving
// the version or queuing a job; a retry of the same job finds the comment
// it already wrote and writes nothing (section 5.7).
func TestAddAgentCommentOncePerJob(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		task := f.create(t, nil)
		jobsBefore, outboxBefore, wakesBefore := f.jobs(t, task), f.outbox(t, task.ID), f.wakes.Load()
		job := uuid.New()
		run := domain.AgentRunRecord{JobID: job, Agent: domain.AgentBlocker, Reasoning: "No update in two days"}

		id, created, err := f.svc.AddAgentComment(ctx, blockerAgent, tenant, task.ID, "  Is anything blocking this?  ", run)
		if err != nil || !created || id == uuid.Nil {
			t.Fatalf("AddAgentComment = %s, %v, %v", id, created, err)
		}
		comments := f.comments(t, task.ID)
		if len(comments) != 1 || comments[0].ID != id || comments[0].AuthorID != blockerAgent.ID || comments[0].Body != "Is anything blocking this?" {
			t.Errorf("comments = %+v", comments)
		}
		stored := f.get(t, task.ID)
		if stored.Version != task.Version {
			t.Errorf("a comment moved the version to %d", stored.Version)
		}
		runs := stored.AgentMetadata.Runs
		if len(runs) != 1 || runs[0].JobID != job || runs[0].Changes[domain.RunCommentID] != id.String() ||
			runs[0].Reasoning != run.Reasoning || !runs[0].At.Equal(f.now) {
			t.Errorf("runs = %+v", runs)
		}
		want := slices.Concat(outboxBefore, []string{"task.comment_added>usr_bob", "task.comment_added>usr_carol"})
		slices.Sort(want)
		if got := f.outbox(t, task.ID); !slices.Equal(got, want) {
			t.Errorf("outbox = %v, want %v", got, want)
		}
		if got := f.jobs(t, task); !slices.Equal(got, jobsBefore) {
			t.Errorf("an agent's comment queued jobs: %v", got)
		}
		if f.wakes.Load() != wakesBefore+1 {
			t.Errorf("wakes = %d, want one more", f.wakes.Load()-wakesBefore)
		}

		// The job runs again: the same comment, and nothing new.
		again, created, err := f.svc.AddAgentComment(ctx, blockerAgent, tenant, task.ID, "Is anything blocking this?",
			domain.AgentRunRecord{JobID: job, Agent: domain.AgentBlocker})
		if err != nil || created || again != id {
			t.Errorf("retry = %s, %v, %v; want the first comment, not created", again, created, err)
		}
		if n := len(f.comments(t, task.ID)); n != 1 {
			t.Errorf("comments after the retry = %d", n)
		}
		if n := len(f.get(t, task.ID).AgentMetadata.Runs); n != 1 {
			t.Errorf("runs after the retry = %d", n)
		}
		if got := f.outbox(t, task.ID); !slices.Equal(got, want) {
			t.Errorf("outbox after the retry = %v", got)
		}
		if f.wakes.Load() != wakesBefore+1 {
			t.Error("a retry that wrote nothing woke the relay")
		}

		// Another job may comment too.
		if _, created, err := f.svc.AddAgentComment(ctx, blockerAgent, tenant, task.ID, "Still stuck?",
			domain.AgentRunRecord{JobID: uuid.New(), Agent: domain.AgentBlocker}); err != nil || !created {
			t.Errorf("second job's comment: %v, %v", created, err)
		}
		if n := len(f.comments(t, task.ID)); n != 2 {
			t.Errorf("comments = %d, want 2", n)
		}
	})
}

func TestAddAgentCommentRefusals(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		run := domain.AgentRunRecord{JobID: uuid.New(), Agent: domain.AgentBlocker}
		for name, tc := range map[string]struct {
			actor  domain.Actor
			tenant string
			id     uuid.UUID
			body   string
			want   error
		}{
			"a person":       {alice, tenant, task.ID, "Hi", domain.ErrValidation},
			"an empty body":  {blockerAgent, tenant, task.ID, "   ", domain.ErrValidation},
			"another tenant": {blockerAgent, "tenant_other", task.ID, "Hi", domain.ErrNotFound},
			"no such task":   {blockerAgent, tenant, uuid.New(), "Hi", domain.ErrNotFound},
		} {
			if _, _, err := f.svc.AddAgentComment(ctx, tc.actor, tc.tenant, tc.id, tc.body, run); !errors.Is(err, tc.want) {
				t.Errorf("%s: error = %v, want %v", name, err, tc.want)
			}
		}
		if n := len(f.comments(t, task.ID)); n != 0 {
			t.Errorf("a refused comment was stored: %d", n)
		}
		if n := len(f.get(t, task.ID).AgentMetadata.Runs); n != 0 {
			t.Errorf("a refused comment recorded a run: %d", n)
		}
	})
}

// TestAgentCommentFailuresLeaveNoPartialChange: the comment, the recorded
// run, and the notifications are written in one transaction.
func TestAgentCommentFailuresLeaveNoPartialChange(t *testing.T) {
	everyFailure(t, "an agent's comment", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		task := f.create(t, nil)
		return []uuid.UUID{task.ID}, func(s *TaskService) error {
			_, _, err := s.AddAgentComment(ctx, blockerAgent, tenant, task.ID, "Any blockers?",
				domain.AgentRunRecord{JobID: uuid.New(), Agent: domain.AgentBlocker})
			return err
		}
	})
}
