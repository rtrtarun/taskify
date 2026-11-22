package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
)

// fakeTool records the calls that reached Execute.
type fakeTool struct {
	name   string
	params string
	tier   func(args json.RawMessage) domain.SafetyTier
	err    error

	mu    sync.Mutex
	calls []call
}

type call struct {
	scope Scope
	args  string
}

func (f *fakeTool) Name() string                { return f.name }
func (f *fakeTool) Description() string         { return "fake " + f.name }
func (f *fakeTool) Parameters() json.RawMessage { return json.RawMessage(f.params) }
func (f *fakeTool) Tier(args json.RawMessage) domain.SafetyTier {
	if f.tier == nil {
		return domain.Tier1
	}
	return f.tier(args)
}
func (f *fakeTool) Execute(_ context.Context, s Scope, args json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{s, string(args)})
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"ok": true}, nil
}
func (f *fakeTool) ran() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// updateTool is shaped like UpdateTaskAttributes: Tier 2 when priority is set.
// The fakes carry their own schemas, so they take names the contract does
// not define.
func updateTool() *fakeTool {
	return &fakeTool{
		name: "FakeUpdate",
		params: `{
			"type": "object",
			"properties": {
				"task_id": {"type": "string", "format": "uuid"},
				"labels": {"type": "array", "items": {"type": "string"}, "maxItems": 5},
				"priority": {"enum": ["low", "medium", "high", "critical"]},
				"reasoning": {"type": "string", "minLength": 1}
			},
			"required": ["task_id", "reasoning"],
			"additionalProperties": false
		}`,
		tier: func(args json.RawMessage) domain.SafetyTier {
			var a struct{ Priority *string }
			_ = json.Unmarshal(args, &a)
			if a.Priority != nil {
				return domain.Tier2
			}
			return domain.Tier1
		},
	}
}

func linkTool() *fakeTool {
	return &fakeTool{name: "FakeLink", params: `{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "format": "uuid"},
			"depends_on_task_ids": {"type": "array", "items": {"type": "string", "format": "uuid"}, "minItems": 1}
		},
		"required": ["task_id", "depends_on_task_ids"]
	}`}
}

type env struct {
	reg          *Registry
	update, link *fakeTool
	noArgs       *fakeTool
	b            repotest.Backend
	task, child  *domain.Task
	sibling      *domain.Task // same tenant, unrelated
	foreign      *domain.Task // another tenant
	scope        Scope
}

func newEnv(t *testing.T) *env {
	t.Helper()
	b := repotest.Memory()
	e := &env{b: b, update: updateTool(), link: linkTool(),
		noArgs: &fakeTool{name: "Ping", params: `{"type": "object", "properties": {}}`}}
	for _, tenant := range []string{"tenant_acme", "tenant_other"} {
		w, _ := domain.NewWorkspace(tenant, "ws", "WS", t0)
		if err := b.Workspaces.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(tenant string, parent *uuid.UUID) *domain.Task {
		task, _, err := domain.NewTask(domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws", Title: "T"}, domain.User("usr_alice"), t0)
		if err != nil {
			t.Fatal(err)
		}
		task.ParentID = parent
		if err := b.Tasks.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	e.task = mk("tenant_acme", nil)
	e.child = mk("tenant_acme", &e.task.ID)
	e.sibling = mk("tenant_acme", nil)
	e.foreign = mk("tenant_other", nil)

	e.reg = NewRegistry(b.Tasks)
	for _, tool := range []Tool{e.update, e.link, e.noArgs} {
		if err := e.reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	version := 1
	job := &domain.AgentJob{ID: uuid.New(), TenantID: "tenant_acme", TaskID: &e.task.ID, AgentType: domain.AgentTriage, BaseTaskVersion: &version}
	settings := domain.DefaultAgentSettings()
	settings.RequireHITLForAll = false
	e.scope = NewScope(job, settings, []string{"FakeUpdate", "FakeLink", "Ping"})
	return e
}

func toolCall(name string, args any) llm.ToolCall {
	text, ok := args.(string)
	if !ok {
		b, _ := json.Marshal(args)
		text = string(b)
	}
	return llm.ToolCall{ID: "call_1", Name: name, Arguments: text}
}

func wantRejected(t *testing.T, name string, err error, code string) {
	t.Helper()
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != code || !errors.Is(err, ErrRejected) {
		t.Errorf("%s: error = %v, want code %s", name, err, code)
	}
}

func TestTier1Executes(t *testing.T) {
	e := newEnv(t)
	res, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", `{ "task_id": "`+e.task.ID.String()+`", "labels": ["bug"], "reasoning": "crash" }`))
	if err != nil || !res.Executed() || res.Tier != domain.Tier1 || res.Output == nil {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	c := e.update.calls[0]
	if c.scope.TenantID != "tenant_acme" || *c.scope.TaskID != e.task.ID || c.scope.BaseVersion != 1 || c.scope.JobID != e.scope.JobID {
		t.Errorf("scope = %+v", c.scope)
	}
	if a := c.scope.Actor(); a.Kind != domain.ActorAgent || a.ID != "agent:triage" {
		t.Errorf("actor = %+v", a)
	}
	// Arguments reach the tool compacted.
	if strings.Contains(c.args, " ") {
		t.Errorf("args = %q", c.args)
	}
	// A child of the job's task is in scope too.
	if _, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", map[string]any{"task_id": e.child.ID, "reasoning": "x"})); err != nil {
		t.Errorf("child task: %v", err)
	}
}

func TestTier2IsProposed(t *testing.T) {
	e := newEnv(t)
	res, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", map[string]any{"task_id": e.task.ID, "priority": "high", "reasoning": "outage"}))
	if err != nil || res.Executed() || res.Tier != domain.Tier2 {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	p := res.Proposal
	if p.Tool != "FakeUpdate" || p.CallID != "call_1" || !strings.Contains(string(p.Arguments), `"priority":"high"`) {
		t.Errorf("proposal = %+v (%s)", p, p.Arguments)
	}
	if e.update.ran() != 0 {
		t.Error("a Tier 2 call ran")
	}

	// A tool reporting a tier other than 1 or 2 is treated as Tier 2.
	odd := &fakeTool{name: "Odd", params: `{"type":"object"}`, tier: func(json.RawMessage) domain.SafetyTier { return 0 }}
	_ = e.reg.Register(odd)
	if res, err := e.reg.Dispatch(ctx, Scope{TenantID: "tenant_acme"}, toolCall("Odd", "{}")); err != nil || res.Executed() || odd.ran() != 0 {
		t.Errorf("unknown tier = %+v, %v", res, err)
	}
}

func TestRequireHITLTurnsTier1IntoProposal(t *testing.T) {
	e := newEnv(t)
	e.scope.RequireHITL = true
	res, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", map[string]any{"task_id": e.task.ID, "labels": []string{"bug"}, "reasoning": "x"}))
	if err != nil || res.Executed() || res.Tier != domain.Tier2 || res.Proposal == nil {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	if e.update.ran() != 0 {
		t.Error("a call ran although approval is required for everything")
	}
	// A new workspace's defaults require approval.
	job := &domain.AgentJob{TenantID: "tenant_acme", AgentType: domain.AgentTriage}
	if !NewScope(job, domain.DefaultAgentSettings(), nil).RequireHITL {
		t.Error("default settings do not require approval")
	}
}

func TestUnknownAndDisallowedTools(t *testing.T) {
	e := newEnv(t)
	_, err := e.reg.Dispatch(ctx, e.scope, toolCall("DropDatabase", "{}"))
	wantRejected(t, "unknown", err, CodeUnknownTool)

	e.scope.AllowedTools = []string{"FakeLink"}
	_, err = e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", map[string]any{"task_id": e.task.ID, "reasoning": "x"}))
	wantRejected(t, "not offered", err, CodeNotAllowed)
	if e.update.ran() != 0 {
		t.Error("a disallowed tool ran")
	}
}

func TestBadArguments(t *testing.T) {
	e := newEnv(t)
	id := e.task.ID.String()
	for name, args := range map[string]string{
		"malformed JSON":     `{"task_id": "` + id + `", `,
		"trailing data":      `{"task_id": "` + id + `", "reasoning": "x"} {}`,
		"not an object":      `["` + id + `"]`,
		"missing required":   `{"task_id": "` + id + `"}`,
		"wrong type":         `{"task_id": "` + id + `", "reasoning": "x", "labels": "bug"}`,
		"unknown property":   `{"task_id": "` + id + `", "reasoning": "x", "assignee_id": "usr_eve"}`,
		"not in enum":        `{"task_id": "` + id + `", "reasoning": "x", "priority": "urgent"}`,
		"too many items":     `{"task_id": "` + id + `", "reasoning": "x", "labels": ["a","b","c","d","e","f"]}`,
		"task_id not a uuid": `{"task_id": "../../etc", "reasoning": "x"}`,
		"empty for required": ``,
	} {
		_, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", args))
		wantRejected(t, name, err, CodeInvalidArguments)
	}
	if e.update.ran() != 0 {
		t.Error("a call with bad arguments ran")
	}

	// The message tells the model what to fix.
	_, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", `{"task_id": "`+id+`"}`))
	if !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("message = %v", err)
	}

	// A tool without parameters accepts empty arguments.
	if res, err := e.reg.Dispatch(ctx, e.scope, toolCall("Ping", "")); err != nil || !res.Executed() {
		t.Errorf("empty arguments = %+v, %v", res, err)
	}
}

func TestTasksOutsideTheScope(t *testing.T) {
	e := newEnv(t)
	for name, id := range map[string]uuid.UUID{
		"another tenant's task":  e.foreign.ID,
		"an unrelated task":      e.sibling.ID,
		"a task that is missing": uuid.New(),
	} {
		_, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", map[string]any{"task_id": id, "reasoning": "x"}))
		wantRejected(t, name, err, CodeOutOfScope)
	}
	// Another tenant's task looks exactly like a missing one.
	_, errForeign := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", map[string]any{"task_id": e.foreign.ID, "reasoning": "x"}))
	if strings.Contains(errForeign.Error(), "tenant") {
		t.Errorf("message mentions the tenant: %v", errForeign)
	}

	// A dependency may name any task in the tenant, but not one outside it.
	if _, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeLink", map[string]any{"task_id": e.task.ID, "depends_on_task_ids": []uuid.UUID{e.sibling.ID}})); err != nil {
		t.Errorf("same-tenant dependency: %v", err)
	}
	_, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeLink", map[string]any{"task_id": e.task.ID, "depends_on_task_ids": []uuid.UUID{e.sibling.ID, e.foreign.ID}}))
	wantRejected(t, "cross-tenant dependency", err, CodeOutOfScope)
	if e.update.ran() != 0 || e.link.ran() != 1 {
		t.Errorf("ran: update %d, link %d", e.update.ran(), e.link.ran())
	}

	// A job without a task is limited to its tenant.
	tenantScope := e.scope
	tenantScope.TaskID = nil
	if _, err := e.reg.Dispatch(ctx, tenantScope, toolCall("FakeUpdate", map[string]any{"task_id": e.sibling.ID, "reasoning": "x"})); err != nil {
		t.Errorf("tenant-scoped job, own tenant: %v", err)
	}
	_, err = e.reg.Dispatch(ctx, tenantScope, toolCall("FakeUpdate", map[string]any{"task_id": e.foreign.ID, "reasoning": "x"}))
	wantRejected(t, "tenant-scoped job, other tenant", err, CodeOutOfScope)
}

func TestToolErrorsPassThrough(t *testing.T) {
	e := newEnv(t)
	e.update.err = domain.ErrVersionConflict
	_, err := e.reg.Dispatch(ctx, e.scope, toolCall("FakeUpdate", map[string]any{"task_id": e.task.ID, "reasoning": "x"}))
	if !errors.Is(err, domain.ErrVersionConflict) || errors.Is(err, ErrRejected) {
		t.Errorf("error = %v", err)
	}
}

func TestRegisterAndSchemas(t *testing.T) {
	e := newEnv(t)
	if err := e.reg.Register(updateTool()); err == nil {
		t.Error("duplicate name accepted")
	}
	for name, params := range map[string]string{
		"not JSON":       `{`,
		"not an object":  `{"type": "array"}`,
		"bad schema":     `{"type": "object", "properties": {"a": {"type": 5}}}`,
		"no type at all": `{}`,
	} {
		if err := e.reg.Register(&fakeTool{name: "X" + name, params: params}); err == nil {
			t.Errorf("%s: schema accepted", name)
		}
	}
	if err := e.reg.Register(&fakeTool{params: `{"type":"object"}`}); err == nil {
		t.Error("unnamed tool accepted")
	}

	schemas, err := e.reg.Schemas([]string{"FakeLink", "FakeUpdate"})
	if err != nil || len(schemas) != 2 || schemas[0].Name != "FakeLink" || schemas[1].Description != "fake FakeUpdate" ||
		!json.Valid(schemas[1].Parameters) {
		t.Errorf("Schemas = %+v, %v", schemas, err)
	}
	if _, err := e.reg.Schemas([]string{"Nope"}); err == nil {
		t.Error("unknown tool in Schemas")
	}
}
