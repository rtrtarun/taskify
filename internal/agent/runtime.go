package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

// Run history outcomes, one per turn.
const (
	OutcomeFinal           = "final"            // the model answered without a tool call
	OutcomeToolCall        = "tool_call"        // every call ran
	OutcomeProposal        = "proposal"         // at least one call was stored for approval
	OutcomeRejected        = "rejected"         // at least one call failed validation
	OutcomeVersionConflict = "version_conflict" // a person changed the task meanwhile
	OutcomeToolError       = "tool_error"       // a tool failed
	OutcomeLLMError        = "llm_error"        // the model could not be reached
)

// How a run ended, in execution_result.
const (
	stoppedFinal       = "final"
	stoppedMaxTurns    = "max_turns"
	stoppedConflict    = "version_conflict"
	stoppedError       = "error"
	stoppedFailed      = "failed"
	stoppedInterrupted = "interrupted"
)

// Config tunes the runtime. Zero values take the defaults in G11.
type Config struct {
	Workers      int           // concurrent jobs; default 4 (AGENT_WORKERS)
	MaxTurns     int           // model turns per run; default 6 (AGENT_MAX_TURNS)
	MaxAttempts  int           // failed runs before a job fails; default 3 (AGENT_MAX_ATTEMPTS)
	PollInterval time.Duration // default 1s
	// StuckAfter is how long a running job may go without a heartbeat before
	// it is recovered; default 5m. The runtime beats after every turn.
	StuckAfter time.Duration
	// RecoverEvery is how often stuck jobs are looked for; default 1m.
	RecoverEvery time.Duration
	// RetryBackoff is the wait after failed attempt n (1-based; the last
	// entry repeats). Default 30s, 2m, 10m.
	RetryBackoff []time.Duration
	// ConflictDelay is the wait before re-running after a version conflict;
	// default 10s.
	ConflictDelay time.Duration
	// FreeConflicts is how many version conflicts re-run without using an
	// attempt; each one after that uses one. Default 5.
	FreeConflicts int
	Now           func() time.Time
	Logger        *slog.Logger
	// Metrics may be nil.
	Metrics *observability.Metrics
	// PublicBaseURL and OutboxMaxAttempts shape approval notifications, as
	// for TaskService.
	PublicBaseURL     string
	OutboxMaxAttempts int
	// Wake is called after an approval request is committed, so the relay
	// sends it at once. Nil does nothing.
	Wake func()
}

func (c *Config) defaults() {
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.MaxTurns <= 0 {
		c.MaxTurns = 6
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.StuckAfter <= 0 {
		c.StuckAfter = 5 * time.Minute
	}
	if c.RecoverEvery <= 0 {
		c.RecoverEvery = time.Minute
	}
	if len(c.RetryBackoff) == 0 {
		c.RetryBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	}
	if c.ConflictDelay <= 0 {
		c.ConflictDelay = 10 * time.Second
	}
	if c.FreeConflicts <= 0 {
		c.FreeConflicts = 5
	}
	if c.OutboxMaxAttempts <= 0 {
		c.OutboxMaxAttempts = 10
	}
	if c.Wake == nil {
		c.Wake = func() {}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// backoff returns the wait after failed attempt n.
func (c *Config) backoff(n int) time.Duration {
	return c.RetryBackoff[min(max(n, 1), len(c.RetryBackoff))-1]
}

// Deps are what the runtime uses.
type Deps struct {
	// Tx and Outbox write a job's approval request with the job's update.
	Tx         repository.TxManager
	Outbox     repository.OutboxRepository
	Jobs       repository.AgentJobRepository
	Tasks      repository.TaskRepository
	Workspaces repository.WorkspaceRepository
	LLM        llm.Client
	Tools      *tools.Registry
}

// Runtime claims and runs agent jobs. Any number of runtimes may share a
// database: a job is claimed by one of them with SKIP LOCKED, and its
// updated_at lease stops a worker whose job was recovered from writing it.
type Runtime struct {
	cfg    Config
	deps   Deps
	agents map[string]Agent
	types  []string
}

// New returns a runtime for the given agents. Each must have a known,
// unique type and offer only registered tools. Jobs for agent types not
// given here are left pending for a runtime that has them.
func New(deps Deps, cfg Config, agents ...Agent) (*Runtime, error) {
	cfg.defaults()
	if deps.Tx == nil || deps.Outbox == nil || deps.Jobs == nil || deps.Tasks == nil ||
		deps.Workspaces == nil || deps.LLM == nil || deps.Tools == nil {
		return nil, errors.New("agent: every dependency is required")
	}
	r := &Runtime{cfg: cfg, deps: deps, agents: map[string]Agent{}}
	for _, a := range agents {
		t := a.Type()
		if !domain.ValidAgentType(t) {
			return nil, fmt.Errorf("agent: unknown agent type %q", t)
		}
		if _, dup := r.agents[t]; dup {
			return nil, fmt.Errorf("agent: %s registered twice", t)
		}
		if _, err := deps.Tools.Schemas(a.AllowedTools()); err != nil {
			return nil, fmt.Errorf("agent: %s: %w", t, err)
		}
		r.agents[t] = a
		r.types = append(r.types, t)
	}
	slices.Sort(r.types)
	return r, nil
}

func (r *Runtime) now() time.Time {
	// PostgreSQL keeps microseconds; the lease compares exactly.
	return r.cfg.Now().UTC().Truncate(time.Microsecond)
}

// Run claims and runs jobs until ctx is cancelled, and recovers stuck jobs.
// Jobs in flight when ctx is cancelled return to pending without using an
// attempt.
func (r *Runtime) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.recoverLoop(ctx)
	}()

	slots := make(chan struct{}, r.cfg.Workers)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if free := cap(slots) - len(slots); free > 0 {
			jobs, err := r.deps.Jobs.Claim(ctx, r.now(), free, r.types)
			if err != nil && ctx.Err() == nil {
				r.cfg.Logger.Error("agent runtime: claim failed", "error", err)
			}
			for _, job := range jobs {
				slots <- struct{}{} // never blocks: at most free jobs were claimed
				wg.Add(1)
				go func() {
					defer func() { <-slots; wg.Done() }()
					r.Execute(ctx, job)
				}()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce claims up to Workers jobs and runs them, returning how many it
// claimed. Tests and tools use it; Run is the long-running form.
func (r *Runtime) RunOnce(ctx context.Context) (int, error) {
	jobs, err := r.deps.Jobs.Claim(ctx, r.now(), r.cfg.Workers, r.types)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Execute(ctx, job)
		}()
	}
	wg.Wait()
	return len(jobs), nil
}

func (r *Runtime) recoverLoop(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.RecoverEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, _, err := r.Recover(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.Error("agent runtime: recovery failed", "error", err)
		}
	}
}

// Recover returns jobs abandoned in running to pending, or fails them at the
// attempt limit (G11).
func (r *Runtime) Recover(ctx context.Context) (requeued, failed int, err error) {
	now := r.now()
	requeued, failed, err = r.deps.Jobs.RecoverStuck(ctx, now.Add(-r.cfg.StuckAfter), now, r.cfg.MaxAttempts)
	if requeued+failed > 0 {
		r.cfg.Logger.Warn("agent runtime: recovered stuck jobs", "requeued", requeued, "failed", failed)
	}
	return requeued, failed, err
}

// permanentError fails a job at once, without further attempts.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

func permanent(format string, a ...any) error {
	return &permanentError{msg: fmt.Sprintf(format, a...)}
}

// Execute runs one claimed job to its next status. The run continues the
// trace of the request whose change queued the job.
func (r *Runtime) Execute(ctx context.Context, job *domain.AgentJob) {
	start := time.Now()
	var in struct {
		TraceContext map[string]string `json:"trace_context"`
	}
	_ = json.Unmarshal(job.InputContext, &in)
	ctx = observability.ExtractTraceContext(ctx, in.TraceContext)
	ctx, span := observability.Tracer().Start(ctx, "agent.job", trace.WithAttributes(
		attribute.String("agent.type", job.AgentType), attribute.String("agent.job_id", job.ID.String())))
	defer span.End()

	log := r.cfg.Logger.With("job_id", job.ID, "tenant_id", job.TenantID, "agent_type", job.AgentType)
	if job.TaskID != nil {
		log = log.With("task_id", *job.TaskID)
	}
	if id := observability.TraceID(ctx); id != "" {
		log = log.With("trace_id", id)
	}
	ru := &run{rt: r, job: job, lease: job.UpdatedAt, log: log}
	err := ru.do(ctx)
	outcome := r.finish(ctx, ru, err)
	if outcome == "" {
		return // the run was dropped
	}
	r.cfg.Metrics.ObserveAgentJob(job.AgentType, outcome, time.Since(start))
	span.SetAttributes(attribute.String("agent.outcome", outcome), attribute.Int("agent.turns", ru.turns))
	if outcome == observability.JobOutcomeFailed {
		span.SetStatus(codes.Error, job.ErrorMessage)
	}
}

// run is one execution of a job.
type run struct {
	rt    *Runtime
	job   *domain.AgentJob
	lease time.Time // the job's updated_at as last written by this run
	log   *slog.Logger
	scope tools.Scope

	turns     int
	repaired  bool
	final     string
	stopped   string
	executed  []executedCall
	proposals []tools.Proposal
}

type executedCall struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

func (ru *run) do(ctx context.Context) error {
	r := ru.rt
	agent := r.agents[ru.job.AgentType]
	if agent == nil {
		return permanent("no agent of type %s is deployed", ru.job.AgentType)
	}
	settings, task, err := ru.load(ctx)
	if err != nil {
		return err
	}
	// Settings are checked again here: a job queued before agents were
	// switched off must not run.
	if !settings.Allows(ru.job.AgentType) {
		return permanent("agent %s is disabled for this workspace", ru.job.AgentType)
	}
	if task != nil {
		// Reason about the task as it is now; a re-run after a version
		// conflict starts from the person's edit.
		v := task.Version
		ru.job.BaseTaskVersion = &v
	}
	allowed := agent.AllowedTools()
	ru.scope = tools.NewScope(ru.job, settings, allowed)
	schemas, err := r.deps.Tools.Schemas(allowed)
	if err != nil {
		return permanent("%v", err)
	}
	messages, err := agent.BuildContext(ctx, ru.job)
	if err != nil {
		return fmt.Errorf("build context: %w", err)
	}

	for ru.turns < r.cfg.MaxTurns {
		ru.turns++
		done, err := ru.turn(ctx, &messages, schemas)
		if err != nil || done {
			return err
		}
		if err := ru.heartbeat(ctx); err != nil {
			return err
		}
	}
	ru.stopped = stoppedMaxTurns
	return nil
}

// load returns the workspace settings that govern the job, and its task.
func (ru *run) load(ctx context.Context) (domain.AgentSettings, *domain.Task, error) {
	d, job := ru.rt.deps, ru.job
	var (
		task        *domain.Task
		workspaceID string
		err         error
	)
	if job.TaskID != nil {
		task, err = d.Tasks.Get(ctx, job.TenantID, *job.TaskID)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.AgentSettings{}, nil, permanent("task %s no longer exists", *job.TaskID)
		}
		if err != nil {
			return domain.AgentSettings{}, nil, err
		}
		workspaceID = task.WorkspaceID
	} else {
		// A job without a task names its workspace in its input context.
		var in struct {
			WorkspaceID string `json:"workspace_id"`
		}
		_ = json.Unmarshal(job.InputContext, &in)
		if in.WorkspaceID == "" {
			return domain.AgentSettings{}, nil, permanent("job has no task and no workspace_id in its input context")
		}
		workspaceID = in.WorkspaceID
	}
	w, err := d.Workspaces.Get(ctx, job.TenantID, workspaceID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.AgentSettings{}, nil, permanent("workspace %s no longer exists", workspaceID)
	}
	if err != nil {
		return domain.AgentSettings{}, nil, err
	}
	return w.AgentSettings, task, nil
}

// turn makes one model call and handles its answer. It reports done when
// the model gave its final answer.
func (ru *run) turn(ctx context.Context, messages *[]llm.Message, schemas []llm.ToolSchema) (bool, error) {
	r := ru.rt
	start := time.Now()
	resp, err := r.deps.LLM.Complete(ctx, llm.Request{Messages: *messages, Tools: schemas})
	latency := time.Since(start)
	if err != nil {
		_ = ru.record(ctx, llm.Response{}, latency, OutcomeLLMError, nil, nil) // the model error is what matters
		return false, fmt.Errorf("model call: %w", err)
	}
	r.cfg.Metrics.AgentTokens(ru.job.AgentType, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	*messages = append(*messages, resp.Message)
	calls := resp.Message.ToolCalls
	if len(calls) == 0 {
		ru.final, ru.stopped = resp.Message.Content, stoppedFinal
		return true, ru.record(ctx, resp, latency, OutcomeFinal, nil, nil)
	}

	outcome := OutcomeToolCall
	results := make([]json.RawMessage, 0, len(calls))
	malformed := false
	for _, call := range calls {
		res, err := r.deps.Tools.Dispatch(ctx, ru.scope, call)
		var rejected *tools.CallError
		var content any
		switch {
		case errors.As(err, &rejected):
			malformed = true
			content = map[string]string{"error": rejected.Code, "message": rejected.Message}
		case errors.Is(err, domain.ErrVersionConflict):
			_ = ru.record(ctx, resp, latency, OutcomeVersionConflict, calls, results) // the conflict is what matters
			return false, err
		case err != nil:
			_ = ru.record(ctx, resp, latency, OutcomeToolError, calls, results) // the tool error is what matters
			return false, fmt.Errorf("tool %s: %w", call.Name, err)
		case !res.Executed():
			ru.proposals = append(ru.proposals, *res.Proposal)
			content = map[string]string{"status": "proposed",
				"message": "Recorded for approval. It runs only after a person approves it."}
		default:
			args := json.RawMessage(call.Arguments)
			if !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			ru.executed = append(ru.executed, executedCall{Tool: call.Name, Arguments: args})
			if v, ok := res.Output.(tools.VersionedOutput); ok && ru.scope.TaskID != nil {
				ru.scope.BaseVersion = v.TaskVersion()
				ru.job.BaseTaskVersion = &ru.scope.BaseVersion
			}
			content = map[string]any{"status": "done", "result": res.Output}
		}
		body, err := json.Marshal(content)
		if err != nil {
			return false, fmt.Errorf("tool %s: encode result: %w", call.Name, err)
		}
		results = append(results, body)
		*messages = append(*messages, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: string(body)})
	}
	switch {
	case malformed:
		outcome = OutcomeRejected
	case len(ru.proposals) > 0:
		outcome = OutcomeProposal
	}
	if err := ru.record(ctx, resp, latency, outcome, calls, results); err != nil {
		return false, err
	}
	if malformed {
		// One repair turn: the rejection is already in the conversation. A
		// second rejection fails the job; the rejected calls changed nothing.
		if ru.repaired {
			return false, permanent("the model's tool calls failed validation after a repair turn")
		}
		ru.repaired = true
	}
	return false, nil
}

// record writes the turn's run history row. Several calls in one turn are
// stored as lists, with tool_name "multiple".
func (ru *run) record(ctx context.Context, resp llm.Response, latency time.Duration, outcome string,
	calls []llm.ToolCall, results []json.RawMessage) error {
	row := domain.AgentRun{
		JobID:            ru.job.ID,
		Turn:             ru.turns,
		Model:            resp.Model,
		Outcome:          outcome,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		LatencyMS:        int(latency.Milliseconds()),
		CreatedAt:        ru.rt.now(),
	}
	id, err := newID()
	if err != nil {
		return err
	}
	row.ID = id
	switch len(calls) {
	case 0:
	case 1:
		row.ToolName = calls[0].Name
		row.ToolArgs = argsJSON(calls[0].Arguments)
		if len(results) == 1 {
			row.ToolResult = results[0]
		}
	default:
		row.ToolName = "multiple"
		list := make([]map[string]any, len(calls))
		for i, c := range calls {
			list[i] = map[string]any{"id": c.ID, "name": c.Name, "arguments": argsJSON(c.Arguments)}
		}
		row.ToolArgs, _ = json.Marshal(list)
		row.ToolResult, _ = json.Marshal(results)
	}
	if err := ru.rt.deps.Jobs.InsertRun(context.WithoutCancel(ctx), row); err != nil {
		return fmt.Errorf("record turn %d: %w", ru.turns, err)
	}
	return nil
}

// argsJSON stores a model's arguments as JSON: the object itself when it is
// valid, or the raw text as a JSON string when it is not.
func argsJSON(text string) json.RawMessage {
	if json.Valid([]byte(text)) {
		return json.RawMessage(text)
	}
	b, _ := json.Marshal(text)
	return b
}

// heartbeat moves the job's lease forward, so a long but healthy run is not
// mistaken for an abandoned one.
func (ru *run) heartbeat(ctx context.Context) error {
	now := ru.rt.now()
	ru.job.UpdatedAt = now
	err := ru.rt.deps.Jobs.Update(ctx, ru.job, domain.JobRunning, ru.lease)
	if errors.Is(err, domain.ErrNotFound) {
		// The job went with its task through a permanent delete.
		return repository.ErrJobMoved
	}
	if err != nil {
		return err
	}
	ru.lease = now
	return nil
}

func newID() (uuid.UUID, error) { return uuid.NewV7() }

type executionResult struct {
	Turns        int            `json:"turns"`
	Stopped      string         `json:"stopped"`
	FinalMessage string         `json:"final_message,omitempty"`
	Executed     []executedCall `json:"executed"`
	Proposals    int            `json:"proposals"`
}

// finish saves the job's next status. Its writes outlive a cancelled ctx,
// so a shutdown still returns the job to pending. Every outcome records what
// the run did, including Tier 1 calls that ran before a failure, since each
// of those committed on its own.
func (r *Runtime) finish(ctx context.Context, ru *run, runErr error) (outcome string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	job, log, now := ru.job, ru.log, r.now()
	var perm *permanentError
	retryIn := func(d time.Duration) {
		at := now.Add(d)
		job.RetryAfter = &at
	}
	job.RetryAfter = nil

	switch {
	case errors.Is(runErr, repository.ErrJobMoved):
		log.Warn("agent runtime: job was taken over; dropping this run", "error", runErr)
		return ""
	case runErr == nil:
		job.ErrorMessage = ""
		next := domain.JobCompleted
		if len(ru.proposals) > 0 {
			// M6-01 adds the approval notification in this same write.
			job.ProposedActions, _ = json.Marshal(ru.proposals)
			next = domain.JobAwaitingApproval
		}
		_ = job.TransitionTo(next, now)
	case ctx.Err() != nil:
		// Shutting down: the job is not at fault.
		ru.stopped = stoppedInterrupted
		job.ErrorMessage = "interrupted by shutdown"
		_ = job.TransitionTo(domain.JobPending, now)
	case errors.Is(runErr, domain.ErrVersionConflict):
		// A person changed the task: run again from their edit after a short
		// wait. The first few conflicts are free; later ones use an attempt,
		// so a task under constant editing cannot keep its job spinning.
		ru.stopped = stoppedConflict
		job.ConflictCount++
		job.ErrorMessage = "version conflict; will run again on the current task"
		next := domain.JobPending
		if job.ConflictCount > r.cfg.FreeConflicts {
			job.Attempts++
			if job.Attempts >= r.cfg.MaxAttempts {
				next = domain.JobFailed
				job.ErrorMessage = "the task kept changing while the agent worked; giving up"
			}
		}
		if next == domain.JobPending {
			retryIn(r.cfg.ConflictDelay)
		}
		_ = job.TransitionTo(next, now)
	case errors.As(runErr, &perm):
		ru.stopped = stoppedFailed
		job.ErrorMessage = perm.msg
		_ = job.TransitionTo(domain.JobFailed, now)
	default:
		ru.stopped = stoppedError
		job.Attempts++
		job.ErrorMessage = runErr.Error()
		next := domain.JobPending
		if job.Attempts >= r.cfg.MaxAttempts {
			next = domain.JobFailed
		} else {
			retryIn(r.cfg.backoff(job.Attempts))
		}
		_ = job.TransitionTo(next, now)
	}

	result := executionResult{Turns: ru.turns, Stopped: ru.stopped, FinalMessage: ru.final,
		Executed: ru.executed, Proposals: len(ru.proposals)}
	if result.Executed == nil {
		result.Executed = []executedCall{}
	}
	job.ExecutionResult, _ = json.Marshal(result)

	if err := r.save(wctx, ctx, ru); err != nil {
		if errors.Is(err, repository.ErrJobMoved) || errors.Is(err, domain.ErrNotFound) {
			log.Warn("agent runtime: job was taken over or deleted; result dropped")
			return ""
		}
		log.Error("agent runtime: could not save job", "error", err)
		return ""
	}
	if job.Status == domain.JobAwaitingApproval {
		r.cfg.Metrics.AgentProposals(job.AgentType, observability.DecisionProposed, len(ru.proposals))
		r.cfg.Wake()
	}
	attrs := []any{"status", job.Status, "turns", ru.turns, "attempts", job.Attempts}
	if job.Status == domain.JobFailed || job.Status == domain.JobPending {
		log.Warn("agent runtime: job did not complete", append(attrs, "error", job.ErrorMessage)...)
	} else {
		log.Info("agent runtime: job finished", attrs...)
	}
	return outcomeOf(job.Status, ru.stopped)
}

// save writes the job's next status. A job that now awaits approval also
// gets its agent.approval_requested outbox row in the same transaction, so
// the request is sent exactly when the proposals are stored. traceCtx carries
// the run's trace for that row.
func (r *Runtime) save(ctx, traceCtx context.Context, ru *run) error {
	job := ru.job
	return r.deps.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := r.deps.Jobs.Update(ctx, job, domain.JobRunning, ru.lease); err != nil {
			return err
		}
		if job.Status != domain.JobAwaitingApproval || job.TaskID == nil {
			// A job without a task has nobody to ask yet; its proposals wait
			// in the job, visible through the API.
			return nil
		}
		task, err := r.deps.Tasks.Get(ctx, job.TenantID, *job.TaskID)
		if err != nil {
			return err
		}
		names := make([]string, len(ru.proposals))
		for i, p := range ru.proposals {
			names[i] = p.Tool
		}
		event := domain.NewApprovalRequestedEvent(task, job, names, job.UpdatedAt)
		entries, err := domain.NewOutboxEntries([]domain.Event{event}, r.cfg.PublicBaseURL, r.cfg.OutboxMaxAttempts, job.UpdatedAt)
		if err != nil {
			return err
		}
		trace := observability.InjectTraceContext(traceCtx)
		for i := range entries {
			entries[i].TraceContext = trace
		}
		_, err = r.deps.Outbox.Insert(ctx, entries)
		return err
	})
}

// outcomeOf is the metric outcome of a run that ended with the job in status.
func outcomeOf(status domain.JobStatus, stopped string) string {
	switch status {
	case domain.JobCompleted:
		return observability.JobOutcomeCompleted
	case domain.JobAwaitingApproval:
		return observability.JobOutcomeAwaiting
	case domain.JobFailed:
		return observability.JobOutcomeFailed
	}
	switch stopped {
	case stoppedConflict:
		return observability.JobOutcomeConflict
	case stoppedInterrupted:
		return observability.JobOutcomeInterrupted
	}
	return observability.JobOutcomeRetry
}
