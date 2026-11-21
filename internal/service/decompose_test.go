package service

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

func TestRequestDecomposition(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)

		// Agents off, then the decomposer alone off: 409 agent_disabled.
		if _, _, err := f.svc.RequestDecomposition(ctx, alice, tenant, task.ID); !errors.Is(err, ErrAgentDisabled) ||
			!strings.Contains(err.Error(), "switched off") {
			t.Errorf("agents off: %v", err)
		}
		f.setAgents(t, domain.AgentSettingsPatch{AgentsEnabled: ptr(true), DisabledAgents: &[]string{domain.AgentDecomposer}})
		if _, _, err := f.svc.RequestDecomposition(ctx, alice, tenant, task.ID); !errors.Is(err, ErrAgentDisabled) ||
			!strings.Contains(err.Error(), "decomposer is disabled") {
			t.Errorf("decomposer off: %v", err)
		}
		if jobs := f.decomposerJobs(t, task); len(jobs) != 0 {
			t.Fatalf("a disabled request queued %v", jobs)
		}

		f.setAgents(t, domain.AgentSettingsPatch{DisabledAgents: &[]string{}})
		job, created, err := f.svc.RequestDecomposition(ctx, alice, tenant, task.ID)
		if err != nil || !created {
			t.Fatalf("RequestDecomposition = %v, %v", created, err)
		}
		if job.AgentType != domain.AgentDecomposer || job.Status != domain.JobPending || *job.TaskID != task.ID ||
			*job.BaseTaskVersion != task.Version || !strings.Contains(string(job.InputContext), `"actor_id":"usr_alice"`) ||
			!strings.Contains(string(job.InputContext), TriggerDecomposeRequest) {
			t.Errorf("job = %+v %s", job, job.InputContext)
		}
		// A repeat while the first is open returns it.
		again, created, err := f.svc.RequestDecomposition(ctx, bob, tenant, task.ID)
		if err != nil || created || again.ID != job.ID {
			t.Errorf("repeat = %v %v %v", again.ID, created, err)
		}
		// Once it has finished, a new request is a new job.
		stored, _ := f.b.AgentJobs.Get(ctx, tenant, job.ID)
		prev := stored.UpdatedAt
		_ = stored.TransitionTo(domain.JobRunning, f.now)
		_ = stored.TransitionTo(domain.JobCompleted, f.now)
		if err := f.b.AgentJobs.Update(ctx, stored, domain.JobPending, prev); err != nil {
			t.Fatal(err)
		}
		if next, created, err := f.svc.RequestDecomposition(ctx, alice, tenant, task.ID); err != nil || !created || next.ID == job.ID {
			t.Errorf("after completion = %v %v", created, err)
		}

		// A closed task, an unknown task, another tenant.
		done := f.setStatus(t, f.create(t, nil), domain.StatusCompleted)
		archived := f.setStatus(t, f.create(t, nil), domain.StatusArchived)
		for _, closed := range []*domain.Task{done, archived} {
			if _, _, err := f.svc.RequestDecomposition(ctx, alice, tenant, closed.ID); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("%s task: %v", closed.Status, err)
			}
			if jobs := f.decomposerJobs(t, closed); len(jobs) != 0 {
				t.Errorf("%s task queued %d jobs", closed.Status, len(jobs))
			}
		}
		// A /decompose comment on a closed task is kept and queues nothing.
		if _, err := f.svc.AddComment(ctx, alice, tenant, archived.ID, "/decompose"); err != nil {
			t.Errorf("comment on an archived task: %v", err)
		}
		if jobs := f.decomposerJobs(t, archived); len(jobs) != 0 {
			t.Errorf("a comment on an archived task queued %d jobs", len(jobs))
		}
		for _, c := range []struct {
			tenant string
			id     uuid.UUID
		}{{tenant, uuid.New()}, {"tenant_other", task.ID}} {
			if _, _, err := f.svc.RequestDecomposition(ctx, alice, c.tenant, c.id); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("%s/%s: %v", c.tenant, c.id, err)
			}
		}
	})
}

func (f *fixture) decomposerJobs(t *testing.T, task *domain.Task) []*domain.AgentJob {
	t.Helper()
	jobs, err := f.b.AgentJobs.ListForTask(ctx, tenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*domain.AgentJob
	for _, j := range jobs {
		if j.AgentType == domain.AgentDecomposer {
			out = append(out, j)
		}
	}
	return out
}

func TestDecomposeComment(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)

		// Agents off: the comment is kept and no job is queued.
		if _, err := f.svc.AddComment(ctx, alice, tenant, task.ID, "/decompose"); err != nil {
			t.Fatal(err)
		}
		if len(f.decomposerJobs(t, task)) != 0 || len(f.comments(t, task.ID)) != 1 {
			t.Fatal("agents off: a job was queued or the comment lost")
		}

		f.enableAgents(t)
		for _, body := range []string{"Please /decompose this", "decompose"} {
			if _, err := f.svc.AddComment(ctx, alice, tenant, task.ID, body); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.svc.AddComment(ctx, decomposer, tenant, task.ID, "/decompose"); err != nil {
			t.Fatal(err)
		}
		if n := len(f.decomposerJobs(t, task)); n != 0 {
			t.Fatalf("%d jobs from comments that are not a person's command", n)
		}

		if _, err := f.svc.AddComment(ctx, bob, tenant, task.ID, " /Decompose "); err != nil {
			t.Fatal(err)
		}
		jobs := f.decomposerJobs(t, task)
		if len(jobs) != 1 {
			t.Fatalf("%d decomposer jobs, want 1", len(jobs))
		}
		var in jobInput
		if err := json.Unmarshal(jobs[0].InputContext, &in); err != nil || in.Trigger != TriggerDecomposeComment || in.ActorID != bob.ID {
			t.Errorf("input context = %s, %v", jobs[0].InputContext, err)
		}
		// The same command again joins the open job.
		if _, err := f.svc.AddComment(ctx, alice, tenant, task.ID, "/decompose"); err != nil {
			t.Fatal(err)
		}
		if n := len(f.decomposerJobs(t, task)); n != 1 {
			t.Errorf("%d jobs after a repeated command", n)
		}
		// The comment is an ordinary comment too.
		if got := f.outbox(t, task.ID); !slices.Contains(got, "task.comment_added>"+bob.ID) {
			t.Errorf("outbox = %v", got)
		}
	})
}

// TestSubtasksCanBeSplitAgain: nothing limits nesting, so a subtask may be
// decomposed like any other open task, and its children point at it.
func TestSubtasksCanBeSplitAgain(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		parent := f.create(t, nil)
		build := byTitle(f.decompose(t, parent, uuid.New(), launchPlan))["Build"]

		if _, created, err := f.svc.RequestDecomposition(ctx, alice, tenant, build.ID); err != nil || !created {
			t.Fatalf("RequestDecomposition on a subtask = %v, %v", created, err)
		}
		grand := f.decompose(t, build, uuid.New(), []domain.SubtaskSpec{{Title: "Compile"}, {Title: "Package", DependsOn: []string{"Compile"}}})
		for _, g := range grand.Subtasks {
			if g.ParentID == nil || *g.ParentID != build.ID {
				t.Errorf("%s parent = %v, want the subtask", g.Title, g.ParentID)
			}
		}
	})
}
