package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rtrtarun/taskify/internal/agent/agenttest"
	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
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
	// everyBuild runs after every context is built.
	everyBuild func()
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
	hook, every := a.afterBuild, a.everyBuild
	a.afterBuild = nil
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	if every != nil {
		every()
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
	if err := f.reg.Register(agenttest.DescribeTool{Tasks: f.svc}); err != nil {
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
	r, err := New(Deps{Tx: f.b.Tx, Outbox: f.b.Outbox, Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg}, cfg, f.agent)
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
		if got.Status != domain.JobPending || got.Attempts != 0 || got.ConflictCount != 1 {
			t.Fatalf("after the conflict: %s, %d attempts, %d conflicts", got.Status, got.Attempts, got.ConflictCount)
		}
		// It waits briefly before running again.
		if got.RetryAfter == nil || got.RetryAfter.Sub(got.UpdatedAt) != 10*time.Second {
			t.Errorf("retry_after = %v, updated %v", got.RetryAfter, got.UpdatedAt)
		}
		var res executionResult
		if err := json.Unmarshal(got.ExecutionResult, &res); err != nil || res.Stopped != stoppedConflict {
			t.Errorf("execution_result = %s", got.ExecutionResult)
		}
		f.clock.advance(10 * time.Second)
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
		waits := []time.Duration{30 * time.Second, 2 * time.Minute, 0}
		for i, want := range []domain.JobStatus{domain.JobPending, domain.JobPending, domain.JobFailed} {
			if _, err := rt.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			got := f.job(t, job.ID)
			if got.Status != want || got.Attempts != i+1 || !strings.Contains(got.ErrorMessage, "model unavailable") {
				t.Errorf("run %d: %s, %d attempts, %q", i+1, got.Status, got.Attempts, got.ErrorMessage)
			}
			if waits[i] == 0 {
				if got.RetryAfter != nil {
					t.Errorf("run %d: a failed job has retry_after %v", i+1, got.RetryAfter)
				}
				continue
			}
			if got.RetryAfter == nil || got.RetryAfter.Sub(got.UpdatedAt) != waits[i] {
				t.Errorf("run %d: retry_after = %v after %v, want +%s", i+1, got.RetryAfter, got.UpdatedAt, waits[i])
			}
			// Not claimed again before the wait is over.
			if n, _ := rt.RunOnce(ctx); n != 0 {
				t.Errorf("run %d: claimed again during its backoff", i+1)
			}
			f.clock.advance(waits[i])
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
			r, err := New(Deps{Tx: b.Tx, Outbox: b.Outbox, Jobs: b.AgentJobs, Tasks: b.Tasks, Workspaces: b.Workspaces, LLM: slowLLM{f.mock}, Tools: f.reg},
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
	deps := Deps{Tx: f.b.Tx, Outbox: f.b.Outbox, Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg}
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

func TestConflictsBeyondTheFreeOnesUseAttempts(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "Busy task")
		// Every run, a person edits the task while the model thinks. The
		// assignee change queues no job of its own.
		f.agent.everyBuild = func() {
			cur, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID)
			who := "usr_" + uuid.NewString()[:8]
			if _, err := f.svc.UpdateTask(ctx, alice, "tenant_acme", task.ID, cur.Version, domain.Patch{AssigneeID: &who}); err != nil {
				t.Error(err)
			}
		}
		for range 4 {
			f.mock.Then(llm.Call("c", "Describe", describe(task.ID, "never lands")))
		}
		rt := f.runtime(t, Config{FreeConflicts: 2, MaxAttempts: 2, ConflictDelay: time.Second})
		want := []struct {
			status    domain.JobStatus
			attempts  int
			conflicts int
		}{
			{domain.JobPending, 0, 1}, {domain.JobPending, 0, 2}, // free
			{domain.JobPending, 1, 3}, {domain.JobFailed, 2, 4}, // counted
		}
		for i, w := range want {
			if _, err := rt.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			got := f.job(t, job.ID)
			if got.Status != w.status || got.Attempts != w.attempts || got.ConflictCount != w.conflicts {
				t.Fatalf("run %d: %s, %d attempts, %d conflicts; want %+v", i+1, got.Status, got.Attempts, got.ConflictCount, w)
			}
			f.clock.advance(time.Second)
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID); stored.Description != "" {
			t.Errorf("a stale write landed: %q", stored.Description)
		}
	})
}

func TestFailedRunRecordsWhatRan(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "T")
		// One write lands, then the model breaks twice: the write stays and
		// the job result says so.
		f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "landed")),
			llm.Call("c2", "Describe", "nope"), llm.Call("c3", "Describe", "still nope"))
		if _, err := f.runtime(t, Config{}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job.ID)
		var res executionResult
		if err := json.Unmarshal(got.ExecutionResult, &res); err != nil || got.Status != domain.JobFailed ||
			res.Stopped != stoppedFailed || len(res.Executed) != 1 || res.Executed[0].Tool != "Describe" {
			t.Errorf("job = %s, result %s", got.Status, got.ExecutionResult)
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID); stored.Description != "landed" {
			t.Errorf("description = %q", stored.Description)
		}
	})
}

func TestReferenceAgentEndToEnd(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		// The person's request carries a trace; the agent's run must join it.
		traceID, _ := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
		spanID, _ := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
		reqCtx := oteltrace.ContextWithSpanContext(ctx, oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: oteltrace.FlagsSampled}))
		task, err := f.svc.CreateTask(reqCtx, alice, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: "Bare task"})
		if err != nil {
			t.Fatal(err)
		}
		jobs, _ := b.AgentJobs.ListForTask(ctx, "tenant_acme", task.ID)
		if len(jobs) != 1 || !strings.Contains(string(jobs[0].InputContext), traceID.String()) {
			t.Fatalf("input_context = %s", jobs[0].InputContext)
		}

		var seenTrace atomic.Value
		ref := tracingAgent{Agent: agenttest.Agent{Tasks: b.Tasks}, seen: &seenTrace}
		metrics := observability.NewMetrics(nil)
		f.mock.Then(llm.Reply(llm.Response{
			Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "Describe", Arguments: `{"task_id":"` + task.ID.String() + `","description":"Written by the agent"}`}}},
			FinishReason: "tool_calls", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20}, Model: "mock"}),
			llm.Reply(llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "Done."}, Usage: llm.Usage{PromptTokens: 150, CompletionTokens: 5}}))
		rt, err := New(Deps{Tx: b.Tx, Outbox: b.Outbox, Jobs: b.AgentJobs, Tasks: b.Tasks, Workspaces: b.Workspaces, LLM: f.mock, Tools: f.reg},
			Config{Logger: quiet, Now: f.clock.now, Metrics: metrics}, ref)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := rt.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}

		// The Tier 1 call changed the task and is in the history.
		stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID)
		if stored.Description != "Written by the agent" {
			t.Errorf("description = %q", stored.Description)
		}
		runs, _ := b.AgentJobs.ListRuns(ctx, jobs[0].ID)
		if len(runs) != 2 || runs[0].ToolName != "Describe" || runs[0].PromptTokens != 100 || runs[1].Outcome != OutcomeFinal {
			t.Errorf("history = %+v", runs)
		}
		if got := seenTrace.Load(); got != traceID.String() {
			t.Errorf("the run's trace = %v, want the request's %s", got, traceID)
		}

		// Metrics.
		body := scrape(t, metrics)
		for _, want := range []string{
			`taskify_agent_job_duration_seconds_count{agent_type="triage",outcome="completed"} 1`,
			`taskify_agent_llm_tokens_total{agent_type="triage",direction="prompt"} 250`,
			`taskify_agent_llm_tokens_total{agent_type="triage",direction="completion"} 25`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("metrics missing %q", want)
			}
		}
	})
}

func TestProposalsAreCounted(t *testing.T) {
	f := newFixture(t, repotest.Memory(), true)
	task, _ := f.task(t, "T")
	metrics := observability.NewMetrics(nil)
	f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "a")), llm.Call("c2", "Describe", describe(task.ID, "b")), llm.Text("ok"))
	rt, err := New(Deps{Tx: f.b.Tx, Outbox: f.b.Outbox, Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg},
		Config{Logger: quiet, Now: f.clock.now, Metrics: metrics}, f.agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, metrics)
	for _, want := range []string{
		`taskify_agent_proposals_total{agent_type="triage",decision="proposed"} 2`,
		`taskify_agent_job_duration_seconds_count{agent_type="triage",outcome="awaiting_approval"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// tracingAgent records the trace ID its context was built under.
type tracingAgent struct {
	agenttest.Agent
	seen *atomic.Value
}

func (a tracingAgent) BuildContext(ctx context.Context, job *domain.AgentJob) ([]llm.Message, error) {
	a.seen.Store(observability.TraceID(ctx))
	return a.Agent.BuildContext(ctx, job)
}

func scrape(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestProposalRequestsApprovalOnce(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, true)
		task, job := f.task(t, "Needs a description") // unassigned: the creator decides
		f.mock.Then(llm.Call("c1", "Describe", describe(task.ID, "proposed")), llm.Text("Proposed."))
		if _, err := f.runtime(t, Config{PublicBaseURL: "https://taskify.test", AppBaseURL: "https://app.test"}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.job(t, job.ID); got.Status != domain.JobAwaitingApproval {
			t.Fatalf("job = %s", got.Status)
		}
		if stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID); stored.Version != 1 || stored.Description != "" {
			t.Errorf("a proposal changed the task: %+v", stored)
		}
		entries, err := b.Outbox.ListForAggregate(ctx, task.ID)
		if err != nil || len(entries) != 1 {
			t.Fatalf("outbox = %+v, %v", entries, err)
		}
		e := entries[0]
		var n domain.Notification
		if err := json.Unmarshal(e.Payload, &n); err != nil {
			t.Fatal(err)
		}
		if e.EventType != domain.EventAgentApprovalRequested || n.UserID != alice.ID ||
			n.Payload["jobId"] != job.ID.String() || !strings.HasSuffix(n.IdempotencyKey, job.ID.String()) {
			t.Errorf("approval notification = %s %+v", e.EventType, n)
		}
		urls, _ := n.Payload["actionUrls"].(map[string]any)
		// view opens the task in the web app (AP-10).
		if urls["approve"] != "https://taskify.test/api/v1/agent-jobs/"+job.ID.String()+"/apply" ||
			urls["view"] != "https://app.test/tasks/"+task.ID.String() {
			t.Errorf("actionUrls = %v", urls)
		}

		// A run that completes without proposals asks nobody.
		f2 := newFixture(t, repotest.Memory(), false)
		task2, _ := f2.task(t, "T")
		f2.mock.Then(llm.Call("c1", "Describe", describe(task2.ID, "done")), llm.Text("ok"))
		if _, err := f2.runtime(t, Config{}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if entries, _ := f2.b.Outbox.ListForAggregate(ctx, task2.ID); len(entries) != 0 {
			for _, e := range entries {
				if e.EventType == domain.EventAgentApprovalRequested {
					t.Errorf("approval requested for a completed job")
				}
			}
		}
	})
}

// toolsAgent is a testAgent offering other tools.
type toolsAgent struct {
	*testAgent
	tools []string
}

func (a toolsAgent) AllowedTools() []string { return a.tools }

func (f *fixture) linkRuntime(t *testing.T) *Runtime {
	t.Helper()
	if err := f.reg.Register(tools.LinkDependencies{Tasks: f.svc}); err != nil {
		t.Fatal(err)
	}
	r, err := New(Deps{Tx: f.b.Tx, Outbox: f.b.Outbox, Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg},
		Config{Logger: quiet, Now: f.clock.now}, toolsAgent{f.agent, []string{tools.LinkDependenciesName, "Describe"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCycleIsReturnedToTheModel(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "Ship the release")
		blocker := f.otherTask(t, "Write release notes")
		// The blocker already waits for the task: linking back closes a cycle.
		if _, err := f.svc.AddDependency(ctx, "tenant_acme", blocker.ID, task.ID); err != nil {
			t.Fatal(err)
		}
		f.mock.Then(
			llm.Call("c1", tools.LinkDependenciesName, map[string]any{"task_id": task.ID, "depends_on_task_ids": []uuid.UUID{blocker.ID}}),
			llm.Text("The link would create a cycle, so I left it."))
		if _, err := f.linkRuntime(t).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := f.job(t, job.ID)
		if got.Status != domain.JobCompleted || got.Attempts != 0 {
			t.Fatalf("job = %s, %d attempts (%s)", got.Status, got.Attempts, got.ErrorMessage)
		}
		if want := []string{OutcomeRuleRejected, OutcomeFinal}; !slices.Equal(f.outcomes(t, job.ID), want) {
			t.Errorf("history = %v", f.outcomes(t, job.ID))
		}
		msgs := f.mock.Requests()[1].Messages
		if last := msgs[len(msgs)-1]; last.Role != llm.RoleTool || !strings.Contains(last.Content, "cycle") {
			t.Errorf("tool result = %+v", last)
		}
		if blockers, _ := b.Deps.ListBlockers(ctx, "tenant_acme", task.ID); len(blockers) != 0 {
			t.Errorf("the cyclic link was made: %v", blockers)
		}
	})
}

func TestLinkThenDescribeInOneRun(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task, job := f.task(t, "Integrate the vendor API")
		blocker := f.otherTask(t, "Get the vendor key")
		// Linking dependency-blocks the todo task (a system change that moves
		// its version); the next call must expect the new version.
		f.mock.Then(
			llm.Call("c1", tools.LinkDependenciesName, map[string]any{"task_id": task.ID, "depends_on_task_ids": []uuid.UUID{blocker.ID}}),
			llm.Call("c2", "Describe", describe(task.ID, "Waits for the vendor key.")),
			llm.Text("Linked and described."))
		if _, err := f.linkRuntime(t).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.job(t, job.ID); got.Status != domain.JobCompleted {
			t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
		}
		stored, _ := b.Tasks.Get(ctx, "tenant_acme", task.ID)
		if stored.Status != domain.StatusBlocked || stored.Description != "Waits for the vendor key." {
			t.Errorf("task = %s %q", stored.Status, stored.Description)
		}
	})
}

// otherTask creates a task as the system would, so it queues no job.
func (f *fixture) otherTask(t *testing.T, title string) *domain.Task {
	t.Helper()
	task, err := f.svc.CreateTask(ctx, domain.System, domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
