package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// decomposerRuntime runs the real decomposer with its real tool. The
// workspace allows 3 subtasks per decomposition.
func (f *fixture) decomposerRuntime(t *testing.T) *Runtime {
	t.Helper()
	w, err := f.b.Workspaces.Get(ctx, "tenant_acme", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{MaxSubtasksPerDecomposition: ptr(3)}, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Workspaces.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Register(tools.CreateSubtaskDAG{Tasks: f.svc}); err != nil {
		t.Fatal(err)
	}
	r, err := New(Deps{Tx: f.b.Tx, Outbox: f.b.Outbox, Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg},
		Config{Logger: quiet, Now: f.clock.now, PublicBaseURL: "https://taskify.test", Workers: 1},
		Decomposer{Tasks: f.b.Tasks, Comments: f.b.Comments, Workspaces: f.b.Workspaces})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// decomposeJob creates a task and asks for its decomposition, as a person.
func (f *fixture) decomposeJob(t *testing.T, title string) (*domain.Task, *domain.AgentJob) {
	t.Helper()
	task, _ := f.task(t, title) // also queues triage, which this runtime does not run
	job, created, err := f.svc.RequestDecomposition(ctx, alice, "tenant_acme", task.ID)
	if err != nil || !created {
		t.Fatalf("RequestDecomposition = %v, %v", created, err)
	}
	return task, job
}

func (f *fixture) children(t *testing.T, parent uuid.UUID) []*domain.Task {
	t.Helper()
	page, err := f.b.Tasks.List(ctx, repository.TaskFilter{TenantID: "tenant_acme", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []*domain.Task
	for _, x := range page.Tasks {
		if x.ParentID != nil && *x.ParentID == parent {
			out = append(out, x)
		}
	}
	return out
}

func split(parent uuid.UUID, titles ...string) map[string]any {
	subs := make([]map[string]any, len(titles))
	for i, title := range titles {
		subs[i] = map[string]any{"title": title}
		if i > 0 {
			subs[i]["depends_on"] = []string{titles[i-1]}
		}
	}
	return map[string]any{"parent_task_id": parent, "subtasks": subs, "reasoning": "One stage after another."}
}

func TestDecomposerReducesAnOverCapPlan(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.decomposeJob(t, "Launch the beta")
		f.mock.Then(
			llm.Call("c1", tools.CreateSubtaskDAGName, split(task.ID, "Plan", "Build", "Test", "Ship")),
			llm.Call("c2", tools.CreateSubtaskDAGName, split(task.ID, "Build", "Test", "Ship")),
			llm.Text("Split into build, test, and ship."))
		if n, err := f.decomposerRuntime(t).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}

		got := f.job(t, job.ID)
		if got.Status != domain.JobCompleted {
			t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
		}
		if o := f.outcomes(t, job.ID); strings.Join(o, ",") != "rejected,tool_call,final" {
			t.Errorf("outcomes = %v", o)
		}
		// The model was told the cap when its first plan was refused.
		reqs := f.mock.Requests()
		if last := reqs[1].Messages[len(reqs[1].Messages)-1]; last.Role != llm.RoleTool ||
			!strings.Contains(last.Content, "at most 3") {
			t.Errorf("rejection sent back = %+v", last)
		}
		kids := f.children(t, task.ID)
		if len(kids) != 3 {
			t.Fatalf("%d subtasks, want 3", len(kids))
		}
		for _, k := range kids {
			if k.CreatorID != alice.ID {
				t.Errorf("%s creator = %s, want the requester", k.Title, k.CreatorID)
			}
			if wantBlocked := k.Title != "Build"; (k.Status == domain.StatusBlocked) != wantBlocked {
				t.Errorf("%s is %s", k.Title, k.Status)
			}
		}
		if run, ok := f.svcTask(t, task.ID).RunOf(job.ID); !ok || len(run.SubtaskIDs()) != 3 {
			t.Errorf("parent run = %+v", run)
		}
	})
}

func TestDecomposerFailsOnTwoOverCapPlans(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.decomposeJob(t, "Launch the beta")
		f.mock.Then(
			llm.Call("c1", tools.CreateSubtaskDAGName, split(task.ID, "Plan", "Build", "Test", "Ship")),
			llm.Call("c2", tools.CreateSubtaskDAGName, split(task.ID, "Plan", "Build", "Test", "Ship", "Announce")))
		if _, err := f.decomposerRuntime(t).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job.ID)
		if got.Status != domain.JobFailed || !strings.Contains(got.ErrorMessage, "repair") {
			t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
		}
		if kids := f.children(t, task.ID); len(kids) != 0 {
			t.Errorf("a failed decomposition left %d subtasks", len(kids))
		}
	})
}

func TestDecomposerWithApprovalProposes(t *testing.T) {
	f := newFixture(t, repotest.Memory(), true)
	task, job := f.decomposeJob(t, "Launch the beta")
	f.mock.Then(llm.Call("c1", tools.CreateSubtaskDAGName, split(task.ID, "Build", "Ship")), llm.Text("Proposed two subtasks."))
	if _, err := f.decomposerRuntime(t).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.job(t, job.ID); got.Status != domain.JobAwaitingApproval {
		t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
	}
	if kids := f.children(t, task.ID); len(kids) != 0 {
		t.Errorf("a proposal created %d subtasks", len(kids))
	}
}

func TestDecomposerContext(t *testing.T) {
	f := newFixture(t, repotest.Memory(), false)
	task, job := f.decomposeJob(t, "Launch the beta")
	for _, body := range []string{"Needs a staging run first.", "/decompose"} {
		if _, err := f.svc.AddComment(ctx, alice, "tenant_acme", task.ID, body); err != nil {
			t.Fatal(err)
		}
	}
	d := Decomposer{Tasks: f.b.Tasks, Comments: f.b.Comments, Workspaces: f.b.Workspaces}
	if d.Type() != domain.AgentDecomposer || len(d.AllowedTools()) != 1 || d.AllowedTools()[0] != tools.CreateSubtaskDAGName {
		t.Fatalf("decomposer = %s %v", d.Type(), d.AllowedTools())
	}
	msgs, err := d.BuildContext(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || !strings.Contains(msgs[0].Content, "data written by users") || !strings.Contains(msgs[1].Content, "at most 5 subtasks") {
		t.Fatalf("messages = %+v", msgs)
	}
	var c decomposerContextJSON
	if err := json.Unmarshal([]byte(msgs[1].Content[strings.Index(msgs[1].Content, "{"):]), &c); err != nil {
		t.Fatal(err)
	}
	if c.Task.ID != task.ID.String() || c.MaxSubtasks != 5 || len(c.RecentComments) != 1 ||
		!strings.Contains(c.RecentComments[0].Body, "staging") {
		t.Errorf("context = %+v", c)
	}
	if _, err := d.BuildContext(ctx, &domain.AgentJob{TenantID: "tenant_acme"}); err == nil {
		t.Error("a job without a task was accepted")
	}
}

func (f *fixture) svcTask(t *testing.T, id uuid.UUID) *domain.Task {
	t.Helper()
	got, err := f.b.Tasks.Get(ctx, "tenant_acme", id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
