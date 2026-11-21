package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
)

var (
	ctx   = context.Background()
	alice = domain.User("usr_alice")
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// clock is real time plus an offset tests can move forward.
type clock struct{ offset atomic.Int64 }

func (c *clock) now() time.Time          { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *clock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// describeTool is a Tier 1 tool that sets a task's description through
// TaskService, with the scope's expected version and the agent as actor.
type describeTool struct{ svc *service.TaskService }

type describeOutput struct {
	Version int `json:"version"`
}

func (o describeOutput) TaskVersion() int { return o.Version }

func (describeTool) Name() string        { return "Describe" }
func (describeTool) Description() string { return "Set the task description." }
func (describeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {
		"task_id": {"type": "string", "format": "uuid"},
		"description": {"type": "string", "minLength": 1}},
		"required": ["task_id", "description"], "additionalProperties": false}`)
}
func (describeTool) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier1 }
func (d describeTool) Execute(ctx context.Context, s tools.Scope, args json.RawMessage) (any, error) {
	var a struct {
		TaskID      uuid.UUID `json:"task_id"`
		Description string    `json:"description"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	task, err := d.svc.UpdateTask(ctx, s.Actor(), s.TenantID, a.TaskID, s.BaseVersion, domain.Patch{Description: &a.Description})
	if err != nil {
		return nil, err
	}
	return describeOutput{Version: task.Version}, nil
}

// testAgent is a triage agent whose context names the task's title and
// version, and which records every context it built.
type testAgent struct {
	tasks interface {
		Get(context.Context, string, uuid.UUID) (*domain.Task, error)
	}
	mu     sync.Mutex
	builds map[uuid.UUID]int
	seen   []string
	// afterBuild runs once, after the first context is built.
	afterBuild func()
}

func (a *testAgent) Type() string           { return domain.AgentTriage }
func (a *testAgent) AllowedTools() []string { return []string{"Describe"} }
func (a *testAgent) BuildContext(ctx context.Context, job *domain.AgentJob) ([]llm.Message, error) {
	task, err := a.tasks.Get(ctx, job.TenantID, *job.TaskID)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.builds[job.ID]++
	a.seen = append(a.seen, task.Title)
	hook := a.afterBuild
	a.afterBuild = nil
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return []llm.Message{
		{Role: llm.RoleSystem, Content: "You triage tasks."},
		{Role: llm.RoleUser, Content: "Task " + task.ID.String() + ": " + task.Title},
	}, nil
}

type fixture struct {
	b     repotest.Backend
	svc   *service.TaskService
	reg   *tools.Registry
	agent *testAgent
	mock  *llm.Mock
	clock *clock
}

func newFixture(t *testing.T, b repotest.Backend, hitl bool) *fixture {
	t.Helper()
	f := &fixture{b: b, mock: llm.NewMock(), clock: &clock{}}
	f.svc = service.NewTaskService(service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments,
		Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs}, service.Config{PublicBaseURL: "https://t"},
		service.WithClock(f.clock.now))
	w, _ := domain.NewWorkspace("tenant_acme", "ws", "WS", f.clock.now())
	if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: ptr(true), RequireHITLForAll: ptr(hitl)}, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := b.Workspaces.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	f.reg = tools.NewRegistry(b.Tasks)
	if err := f.reg.Register(describeTool{f.svc}); err != nil {
		t.Fatal(err)
	}
	f.agent = &testAgent{tasks: b.Tasks, builds: map[uuid.UUID]int{}}
	return f
}

func (f *fixture) runtime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	if cfg.Now == nil {
		cfg.Now = f.clock.now
	}
	cfg.Logger = quiet
	r, err := New(Deps{Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg}, cfg, f.agent)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// task creates a task as a person, which queues one triage job.
func (f *fixture) task(t *testing.T, title string) (*domain.Task, *domain.AgentJob) {
	t.Helper()
	task, err := f.svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: title})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := f.b.AgentJobs.ListForTask(ctx, "tenant_acme", task.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs for a new task = %v, %v", jobs, err)
	}
	return task, jobs[0]
}

func (f *fixture) job(t *testing.T, id uuid.UUID) *domain.AgentJob {
	t.Helper()
	j, err := f.b.AgentJobs.Get(ctx, "tenant_acme", id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (f *fixture) outcomes(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	runs, err := f.b.AgentJobs.ListRuns(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range runs {
		out = append(out, r.Outcome)
	}
	return out
}

func describe(id uuid.UUID, text string) map[string]any {
	return map[string]any{"task_id": id, "description": text}
}

func ptr[T any](v T) *T { return &v }

func each(t *testing.T, fn func(t *testing.T, b repotest.Backend)) {
	t.Helper()
	for _, factory := range repotest.Factories(t) {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			fn(t, factory.New(t))
		})
	}
}

func TestTier1CallsRunAndAreRecorded(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "Checkout crashes")
		// Two writes in one run: the second expects the version the first
		// produced, so the agent does not conflict with itself.
		f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "first")),
			llm.Call("c2", "Describe", describe(task.ID, "second")), llm.Text("Done."))
		if n, err := f.runtime(t, Config{}).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}

		got := f.job(t, job.ID)
		if got.Status != domain.JobCompleted || got.Attempts != 0 || got.ErrorMessage != "" {
			t.Fatalf("job = %s, %d attempts, %q", got.Status, got.Attempts, got.ErrorMessage)
		}
		stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID)
		if stored.Description != "second" || stored.Version != 3 || *got.BaseTaskVersion != 3 {
			t.Errorf("task = %q v%d, base version %d", stored.Description, stored.Version, *got.BaseTaskVersion)
		}
		var res executionResult
		if err := json.Unmarshal(got.ExecutionResult, &res); err != nil || res.Turns != 3 || res.Stopped != stoppedFinal ||
			res.FinalMessage != "Done." || len(res.Executed) != 2 || res.Executed[0].Tool != "Describe" {
			t.Errorf("execution_result = %s, %v", got.ExecutionResult, err)
		}
		if want := []string{OutcomeToolCall, OutcomeToolCall, OutcomeFinal}; !slices.Equal(f.outcomes(t, job.ID), want) {
			t.Errorf("history = %v, want %v", f.outcomes(t, job.ID), want)
		}

		// The model saw the tools and its own tool results.
		reqs := f.mock.Requests()
		if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "Describe" {
			t.Errorf("tools offered = %+v", reqs[0].Tools)
		}
		last := reqs[1].Messages[len(reqs[1].Messages)-1]
		if last.Role != llm.RoleTool || last.ToolCallID != "c1" || !strings.Contains(last.Content, `"done"`) {
			t.Errorf("tool result message = %+v", last)
		}
		// The agent's changes queue no further agent work.
		if jobs, _ := b.AgentJobs.ListForTask(ctx, "tenant_acme", task.ID); len(jobs) != 1 {
			t.Errorf("%d jobs after the agent's changes, want 1", len(jobs))
		}
	})
}

func TestProposalsAwaitApproval(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, true) // approval required for everything
		task, job := f.task(t, "T")
		f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "proposed")), llm.Text("Proposed."))
		if _, err := f.runtime(t, Config{}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job.ID)
		if got.Status != domain.JobAwaitingApproval {
			t.Fatalf("status = %s (%s)", got.Status, got.ErrorMessage)
		}
		var props []tools.Proposal
		if err := json.Unmarshal(got.ProposedActions, &props); err != nil || len(props) != 1 || props[0].Tool != "Describe" || props[0].CallID != "c1" {
			t.Errorf("proposed_actions = %s, %v", got.ProposedActions, err)
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID); stored.Description != "" || stored.Version != 1 {
			t.Errorf("a proposal changed the task: %+v", stored)
		}
		if *got.BaseTaskVersion != 1 {
			t.Errorf("base version = %d", *got.BaseTaskVersion)
		}
	})
}

func TestOneRepairTurn(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		rt := f.runtime(t, Config{})

		// Malformed, then fixed.
		task, job := f.task(t, "Repaired")
		f.mock.Then(llm.Call("c1", "Describe", `{"task_id": "`+task.ID.String()+`"`),
			llm.Call("c2", "Describe", describe(task.ID, "fixed")), llm.Text("ok"))
		if _, err := rt.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.job(t, job.ID); got.Status != domain.JobCompleted {
			t.Errorf("repaired job = %s (%s)", got.Status, got.ErrorMessage)
		}
		// The rejection went back to the model.
		if msgs := f.mock.Requests()[1].Messages; !strings.Contains(msgs[len(msgs)-1].Content, "invalid_arguments") {
			t.Errorf("repair message = %+v", msgs[len(msgs)-1])
		}

		// Malformed twice: the job fails at once and nothing changed.
		task2, job2 := f.task(t, "Broken")
		f.mock.Then(llm.Call("c1", "Describe", `not json`), llm.Call("c2", "NoSuchTool", "{}"))
		if _, err := rt.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job2.ID)
		if got.Status != domain.JobFailed || got.Attempts != 0 || !strings.Contains(got.ErrorMessage, "repair") {
			t.Errorf("job = %s, %d attempts, %q", got.Status, got.Attempts, got.ErrorMessage)
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task2.ID); stored.Version != 1 {
			t.Errorf("task changed: v%d", stored.Version)
		}
		if want := []string{OutcomeRejected, OutcomeRejected}; !slices.Equal(f.outcomes(t, job2.ID), want) {
			t.Errorf("history = %v", f.outcomes(t, job2.ID))
		}
	})
}

func TestVersionConflictReRunsWithFreshContext(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "Original title")
		// While the model thinks, a person renames the task. The rename
		// queues its own triage job, which this runtime leaves for later.
		f.agent.afterBuild = func() {
			if _, err := f.svc.UpdateTask(ctx, alice, "tenant_acme", task.ID, 1, domain.Patch{Title: ptr("Edited by a person")}); err != nil {
				t.Error(err)
			}
		}
		f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "stale")),
			llm.Call("c2", "Describe", describe(task.ID, "fresh")), llm.Text("ok"))
		rt := f.runtime(t, Config{Workers: 1})

		if _, err := rt.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job.ID)
		if got.Status != domain.JobPending || got.Attempts != 0 {
			t.Fatalf("after the conflict: %s, %d attempts", got.Status, got.Attempts)
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID); stored.Description != "" {
			t.Errorf("the stale write landed: %q", stored.Description)
		}

		// The re-run (oldest job first) sees the person's edit and succeeds.
		if _, err := rt.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got = f.job(t, job.ID)
		if got.Status != domain.JobCompleted {
			t.Fatalf("after the re-run: %s (%s)", got.Status, got.ErrorMessage)
		}
		if f.agent.seen[1] != "Edited by a person" || f.agent.builds[job.ID] != 2 {
			t.Errorf("contexts = %v, builds %d", f.agent.seen, f.agent.builds[job.ID])
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID); stored.Description != "fresh" || stored.Title != "Edited by a person" {
			t.Errorf("task = %+v", stored)
		}
		if want := []string{OutcomeVersionConflict, OutcomeToolCall, OutcomeFinal}; !slices.Equal(f.outcomes(t, job.ID), want) {
			t.Errorf("history = %v, want %v", f.outcomes(t, job.ID), want)
		}
	})
}

func TestJobQueuedBeforeAgentsWereSwitchedOff(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "T")
		w, _ := b.Workspaces.Get(ctx, "tenant_acme", "ws")
		if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: ptr(false)}, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		if err := b.Workspaces.Update(ctx, w); err != nil {
			t.Fatal(err)
		}
		f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "should not happen")))
		if _, err := f.runtime(t, Config{}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job.ID)
		if got.Status != domain.JobFailed || !strings.Contains(got.ErrorMessage, "disabled") {
			t.Errorf("job = %s, %q", got.Status, got.ErrorMessage)
		}
		if n := len(f.mock.Requests()); n != 0 || len(f.outcomes(t, job.ID)) != 0 {
			t.Errorf("the model was called %d times", n)
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID); stored.Version != 1 {
			t.Error("the task changed")
		}
	})
}

func TestErrorsCountAttempts(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		_, job := f.task(t, "T")
		down := errors.New("model unavailable")
		f.mock.Then(llm.Fail(down), llm.Fail(down), llm.Fail(down))
		rt := f.runtime(t, Config{MaxAttempts: 3})
		for i, want := range []domain.JobStatus{domain.JobPending, domain.JobPending, domain.JobFailed} {
			if _, err := rt.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			got := f.job(t, job.ID)
			if got.Status != want || got.Attempts != i+1 || !strings.Contains(got.ErrorMessage, "model unavailable") {
				t.Errorf("run %d: %s, %d attempts, %q", i+1, got.Status, got.Attempts, got.ErrorMessage)
			}
		}
		if want := []string{OutcomeLLMError, OutcomeLLMError, OutcomeLLMError}; !slices.Equal(f.outcomes(t, job.ID), want) {
			t.Errorf("history = %v", f.outcomes(t, job.ID))
		}
	})
}

func TestTurnLimit(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "T")
		f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "a")), llm.Call("c2", "Describe", describe(task.ID, "b")),
			llm.Call("c3", "Describe", describe(task.ID, "never")))
		if _, err := f.runtime(t, Config{MaxTurns: 2}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job.ID)
		var res executionResult
		_ = json.Unmarshal(got.ExecutionResult, &res)
		if got.Status != domain.JobCompleted || res.Stopped != stoppedMaxTurns || res.Turns != 2 || f.mock.Remaining() != 1 {
			t.Errorf("job = %s, result %s, %d steps left", got.Status, got.ExecutionResult, f.mock.Remaining())
		}
	})
}

func TestRecoveredJobIsNotOverwritten(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		_, job := f.task(t, "T")
		rt := f.runtime(t, Config{})
		claimed, err := b.AgentJobs.Claim(ctx, rt.now(), 1, []string{domain.AgentTriage})
		if err != nil || len(claimed) != 1 {
			t.Fatal(claimed, err)
		}
		// The worker holding it goes silent; recovery requeues it.
		f.clock.advance(10 * time.Minute)
		if requeued, failed, err := rt.Recover(ctx); err != nil || requeued != 1 || failed != 0 {
			t.Fatalf("Recover = %d, %d, %v", requeued, failed, err)
		}
		// The silent worker comes back and finishes: its result is dropped.
		f.mock.Then(llm.Text("late"))
		rt.Execute(ctx, claimed[0])
		if got := f.job(t, job.ID); got.Status != domain.JobPending || got.Attempts != 1 {
			t.Errorf("job = %s, %d attempts", got.Status, got.Attempts)
		}
	})
}

func TestShutdownReturnsJobWithoutUsingAnAttempt(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		_, job := f.task(t, "T")
		rt := f.runtime(t, Config{})
		claimed, err := b.AgentJobs.Claim(ctx, rt.now(), 1, []string{domain.AgentTriage})
		if err != nil || len(claimed) != 1 {
			t.Fatal(claimed, err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		rt.Execute(cancelled, claimed[0])
		if got := f.job(t, job.ID); got.Status != domain.JobPending || got.Attempts != 0 {
			t.Errorf("job = %s, %d attempts (%s)", got.Status, got.Attempts, got.ErrorMessage)
		}
	})
}

// slowLLM delays each answer so concurrent runtimes overlap.
type slowLLM struct{ inner llm.Client }

func (s slowLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	time.Sleep(3 * time.Millisecond)
	return s.inner.Complete(ctx, req)
}

func TestTwoRuntimesNeverRunTheSameJob(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		const jobs = 16
		var ids []uuid.UUID
		for range jobs {
			_, job := f.task(t, "T")
			ids = append(ids, job.ID)
		}
		for range jobs * 2 {
			f.mock.Then(llm.Text("done"))
		}
		var runtimes []*Runtime
		for range 2 {
			r, err := New(Deps{Jobs: b.AgentJobs, Tasks: b.Tasks, Workspaces: b.Workspaces, LLM: slowLLM{f.mock}, Tools: f.reg},
				Config{Workers: 3, PollInterval: 2 * time.Millisecond, Logger: quiet}, f.agent)
			if err != nil {
				t.Fatal(err)
			}
			runtimes = append(runtimes, r)
		}
		runCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for _, r := range runtimes {
			wg.Add(1)
			go func() { defer wg.Done(); _ = r.Run(runCtx) }()
		}

		deadline := time.Now().Add(30 * time.Second)
		for {
			done := 0
			for _, id := range ids {
				if f.job(t, id).Status == domain.JobCompleted {
					done++
				}
			}
			if done == jobs {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				wg.Wait()
				t.Fatalf("%d of %d jobs completed", done, jobs)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		wg.Wait()

		f.agent.mu.Lock()
		defer f.agent.mu.Unlock()
		for _, id := range ids {
			if n := f.agent.builds[id]; n != 1 {
				t.Errorf("job %s ran %d times", id, n)
			}
		}
	})
}

func TestNewValidatesAgents(t *testing.T) {
	f := newFixture(t, repotest.Memory(), false)
	deps := Deps{Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg}
	if _, err := New(deps, Config{}, f.agent, f.agent); err == nil {
		t.Error("duplicate agent accepted")
	}
	if _, err := New(deps, Config{}, &badAgent{typ: "janitor"}); err == nil {
		t.Error("unknown agent type accepted")
	}
	if _, err := New(deps, Config{}, &badAgent{typ: domain.AgentBlocker, tools: []string{"Unregistered"}}); err == nil {
		t.Error("agent with an unregistered tool accepted")
	}
}

type badAgent struct {
	typ   string
	tools []string
}

func (a *badAgent) Type() string           { return a.typ }
func (a *badAgent) AllowedTools() []string { return a.tools }
func (a *badAgent) BuildContext(context.Context, *domain.AgentJob) ([]llm.Message, error) {
	return nil, nil
}
