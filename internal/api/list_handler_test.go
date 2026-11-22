package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/worker"
)

// The list endpoints for clients (M12-04).

func TestMe(t *testing.T) {
	e := newEnv(t)
	if r := e.do(req{method: "GET", path: "/api/v1/me"}); r.status != 200 || r.json["tenant_id"] != tenant ||
		r.json["user_id"] != "usr_bob" || r.json["role"] != "member" {
		t.Errorf("member: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me", as: "admin"}); r.json["role"] != "admin" || r.json["user_id"] != "usr_admin" {
		t.Errorf("admin: %s", r.raw)
	}
	dev := newEnv(t, func(d *Deps) { d.Auth.Development = true })
	r := dev.do(req{method: "GET", path: "/api/v1/me", as: "none", headers: map[string]string{
		HeaderTenantID: tenant, HeaderUserID: " usr_dev ", HeaderUserRole: "owner"}})
	if r.status != 200 || r.json["user_id"] != "usr_dev" || r.json["role"] != "member" {
		t.Errorf("development headers: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me", as: "none"}); r.status != 401 {
		t.Errorf("no credentials: %d", r.status)
	}
}

func (e *env) insertJob(taskID *uuid.UUID, agent string, at time.Time) *domain.AgentJob {
	e.t.Helper()
	j, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, TaskID: taskID, AgentType: agent, DedupKey: uuid.NewString()}, at)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.b.AgentJobs.Insert(context.Background(), j); err != nil {
		e.t.Fatal(err)
	}
	return &j
}

func TestListAgentJobs(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := uuid.MustParse(task["id"].(string))
	var ids []string // newest first
	for i := 0; i < 3; i++ {
		j := e.insertJob(&id, domain.AgentTriage, e.now.Add(time.Duration(i)*time.Minute))
		ids = append([]string{j.ID.String()}, ids...)
	}
	brief := e.insertJob(nil, domain.AgentDigest, e.now.Add(time.Hour))

	r := e.do(req{method: "GET", path: "/api/v1/agent-jobs?task_id=" + id.String() + "&limit=2"})
	jobs, _ := r.json["agent_jobs"].([]any)
	if r.status != 200 || len(jobs) != 2 || jobs[0].(map[string]any)["id"] != ids[0] || r.json["next_cursor"] == nil {
		t.Fatalf("first page: %d %s", r.status, r.raw)
	}
	r = e.do(req{method: "GET", path: "/api/v1/agent-jobs?task_id=" + id.String() + "&limit=2&cursor=" + r.json["next_cursor"].(string)})
	jobs, _ = r.json["agent_jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["id"] != ids[2] || r.json["next_cursor"] != nil {
		t.Fatalf("second page: %s", r.raw)
	}
	r = e.do(req{method: "GET", path: "/api/v1/agent-jobs?agent_type=digest&status=pending"})
	if jobs, _ := r.json["agent_jobs"].([]any); len(jobs) != 1 || jobs[0].(map[string]any)["id"] != brief.ID.String() ||
		jobs[0].(map[string]any)["task_id"] != nil {
		t.Errorf("digest jobs: %s", r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/agent-jobs?status=awaiting_approval"}); len(r.json["agent_jobs"].([]any)) != 0 {
		t.Errorf("awaiting approval: %s", r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/agent-jobs", as: "other-tenant"}); len(r.json["agent_jobs"].([]any)) != 0 {
		t.Errorf("another tenant sees jobs: %s", r.raw)
	}
	for q, status := range map[string]int{
		"status=done":           422,
		"agent_type=oracle":     422,
		"task_id=nope":          400,
		"limit=0":               400,
		"cursor=%25":            400,
		"limit=200&status=":     200,
		"agent_type=decomposer": 200,
	} {
		if r := e.do(req{method: "GET", path: "/api/v1/agent-jobs?" + q}); r.status != status {
			t.Errorf("%s: %d %s", q, r.status, r.raw)
		}
	}
}

func TestListSubtasks(t *testing.T) {
	e := newEnv(t)
	parent := e.createTask(nil)
	parentID := uuid.MustParse(parent["id"].(string))
	ctx := context.Background()
	var children []*domain.Task
	for i := 0; i < 2; i++ {
		c, _, err := domain.NewTask(domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "Step"}, domain.User("usr_bob"),
			e.now.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		c.ParentID = &parentID
		if err := e.b.Tasks.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
	}
	if _, err := e.b.Deps.Create(ctx, domain.Dependency{TaskID: children[1].ID, DependsOnTaskID: children[0].ID,
		TenantID: tenant, CreatedAt: e.now}); err != nil {
		t.Fatal(err)
	}

	r := e.do(req{method: "GET", path: "/api/v1/tasks/" + parentID.String() + "/subtasks"})
	subtasks, _ := r.json["subtasks"].([]any)
	if r.status != 200 || len(subtasks) != 2 {
		t.Fatalf("subtasks: %d %s", r.status, r.raw)
	}
	first, second := subtasks[0].(map[string]any), subtasks[1].(map[string]any)
	if first["task"].(map[string]any)["id"] != children[0].ID.String() || len(first["depends_on"].([]any)) != 0 {
		t.Errorf("first = %v", first)
	}
	if deps := second["depends_on"].([]any); len(deps) != 1 || deps[0] != children[0].ID.String() {
		t.Errorf("second = %v", second)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks/" + children[0].ID.String() + "/subtasks"}); r.status != 200 ||
		len(r.json["subtasks"].([]any)) != 0 {
		t.Errorf("no children: %d %s", r.status, r.raw)
	}
	for name, rq := range map[string]req{
		"unknown":      {method: "GET", path: "/api/v1/tasks/" + uuid.NewString() + "/subtasks"},
		"malformed":    {method: "GET", path: "/api/v1/tasks/nope/subtasks"},
		"other tenant": {method: "GET", path: "/api/v1/tasks/" + parentID.String() + "/subtasks", as: "other-tenant"},
	} {
		if r := e.do(rq); r.status != 404 {
			t.Errorf("%s: %d", name, r.status)
		}
	}
}

func TestListAgentRuns(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := uuid.MustParse(task["id"].(string))
	job := e.insertJob(&id, domain.AgentTriage, e.now)
	ctx := context.Background()
	for _, run := range []domain.AgentRun{
		{ID: uuid.New(), JobID: job.ID, Turn: 1, Model: "m", ToolName: "UpdateTaskAttributes",
			ToolArgs: json.RawMessage(`{"labels":["bug"],"reasoning":"crash"}`), ToolResult: json.RawMessage(`{"version":2}`),
			Outcome: "executed", PromptTokens: 100, CompletionTokens: 20, LatencyMS: 300, CreatedAt: e.now},
		{ID: uuid.New(), JobID: job.ID, Turn: 2, Model: "m", Outcome: "final", CreatedAt: e.now.Add(time.Second)},
	} {
		if err := e.b.AgentJobs.InsertRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	r := e.do(req{method: "GET", path: "/api/v1/tasks/" + id.String() + "/agent-runs"})
	list, _ := r.json["agent_runs"].([]any)
	if r.status != 200 || len(list) != 1 {
		t.Fatalf("agent runs: %d %s", r.status, r.raw)
	}
	entry := list[0].(map[string]any)
	runs := entry["runs"].([]any)
	if entry["job"].(map[string]any)["id"] != job.ID.String() || len(runs) != 2 {
		t.Fatalf("entry = %v", entry)
	}
	turn1, turn2 := runs[0].(map[string]any), runs[1].(map[string]any)
	if turn1["tool_name"] != "UpdateTaskAttributes" || turn1["tool_args"].(map[string]any)["reasoning"] != "crash" ||
		turn1["prompt_tokens"] != 100.0 || turn2["tool_name"] != nil || turn2["tool_args"] != nil || turn2["outcome"] != "final" {
		t.Errorf("runs = %v", runs)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks/" + uuid.NewString() + "/agent-runs"}); r.status != 404 {
		t.Errorf("unknown task: %d", r.status)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks/" + id.String() + "/agent-runs", as: "other-tenant"}); r.status != 404 {
		t.Errorf("another tenant: %d", r.status)
	}
	if r := newEnv(t, func(d *Deps) { d.AgentJobs = nil }).do(req{method: "GET", path: "/api/v1/tasks/" + id.String() + "/agent-runs"}); r.status != 404 {
		t.Errorf("without agent jobs: %d", r.status)
	}
}

func TestTaskIDPrefix(t *testing.T) {
	e := newEnv(t)
	id := e.createTask(nil)["id"].(string)
	e.createTask(nil)
	for _, prefix := range []string{id, strings.ToUpper(id[:13])} {
		r := e.do(req{method: "GET", path: "/api/v1/tasks?id_prefix=" + prefix})
		tasks, _ := r.json["tasks"].([]any)
		found := false
		for _, task := range tasks {
			found = found || task.(map[string]any)["id"] == id
		}
		if r.status != 200 || !found {
			t.Errorf("%s: %d %s", prefix, r.status, r.raw)
		}
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?id_prefix=" + id}); len(r.json["tasks"].([]any)) != 1 {
		t.Errorf("a whole ID matched %s", r.raw)
	}
	for _, bad := range []string{"xyz", "0193a1b2c3", strings.Repeat("a", 37)} {
		if r := e.do(req{method: "GET", path: "/api/v1/tasks?id_prefix=" + bad}); r.status != 422 || r.errCode() != CodeValidation {
			t.Errorf("%s: %d %s", bad, r.status, r.raw)
		}
	}
}

// TestOutboxAdmin covers GET /admin/outbox and the replay of a dead-letter
// row, which the relay then dispatches.
func TestOutboxAdmin(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil) // one task.created row, to usr_carol
	ctx := context.Background()
	claimed, err := e.b.Outbox.ClaimBatch(ctx, e.now, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d, %v", len(claimed), err)
	}
	row := claimed[0]
	if err := e.b.Outbox.MarkDeadLetter(ctx, row.ID, *row.ClaimedAt, 10, "503 from AgentPulse"); err != nil {
		t.Fatal(err)
	}

	r := e.do(req{method: "GET", path: "/api/v1/admin/outbox?status=dead_letter", as: "admin"})
	entries, _ := r.json["entries"].([]any)
	if r.status != 200 || len(entries) != 1 {
		t.Fatalf("dead letters: %d %s", r.status, r.raw)
	}
	dead := entries[0].(map[string]any)
	if dead["id"] != row.ID.String() || dead["aggregate_id"] != task["id"] || dead["event_type"] != "task.created" ||
		dead["last_error"] != "503 from AgentPulse" || dead["attempts"] != 10.0 || dead["payload"].(map[string]any)["userId"] != "usr_carol" {
		t.Errorf("dead letter = %v", dead)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/admin/outbox?status=pending", as: "admin"}); len(r.json["entries"].([]any)) != 0 {
		t.Errorf("pending: %s", r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/admin/outbox", as: "other-tenant"}); len(r.json["entries"].([]any)) != 0 {
		t.Errorf("another tenant sees rows: %s", r.raw)
	}

	// Replay, then the relay sends it.
	path := "/api/v1/admin/outbox/" + row.ID.String() + "/replay"
	r = e.do(req{method: "POST", path: path, as: "admin"})
	if r.status != 200 || r.json["status"] != "pending" || r.json["attempts"] != 0.0 {
		t.Fatalf("replay: %d %s", r.status, r.raw)
	}
	relay := worker.NewRelay(e.b.Outbox, agentpulse.New(e.apURL, "k", agentpulse.WithBreaker(nil)),
		worker.RelayConfig{Now: func() time.Time { return e.now.Add(time.Second) }, Logger: quiet})
	if n, err := relay.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("relay: %d, %v", n, err)
	}
	r = e.do(req{method: "GET", path: "/api/v1/admin/outbox?status=dispatched", as: "admin"})
	if entries, _ := r.json["entries"].([]any); len(entries) != 1 || entries[0].(map[string]any)["dispatched_at"] == nil {
		t.Errorf("after the relay: %s", r.raw)
	}
	if sent := e.mock.Notifications(); len(sent) != 1 || sent[0].Body["eventType"] != "task.created" {
		t.Errorf("AgentPulse received %v", sent)
	}

	for name, tc := range map[string]struct {
		r      req
		status int
		code   string
	}{
		"replay a dispatched row": {req{method: "POST", path: path, as: "admin"}, 409, CodeNotDeadLetter},
		"another tenant's row":    {req{method: "POST", path: path, as: "other-tenant"}, 404, CodeNotFound},
		"unknown row":             {req{method: "POST", path: "/api/v1/admin/outbox/" + uuid.NewString() + "/replay", as: "admin"}, 404, CodeNotFound},
		"malformed ID":            {req{method: "POST", path: "/api/v1/admin/outbox/nope/replay", as: "admin"}, 404, CodeNotFound},
		"member lists":            {req{method: "GET", path: "/api/v1/admin/outbox"}, 403, CodeForbidden},
		"member replays":          {req{method: "POST", path: path}, 403, CodeForbidden},
		"unknown status":          {req{method: "GET", path: "/api/v1/admin/outbox?status=lost", as: "admin"}, 422, CodeValidation},
		"bad limit":               {req{method: "GET", path: "/api/v1/admin/outbox?limit=201", as: "admin"}, 400, CodeInvalidRequest},
		"bad cursor":              {req{method: "GET", path: "/api/v1/admin/outbox?cursor=bm9wZQ", as: "admin"}, 400, CodeInvalidRequest},
	} {
		if r := e.do(tc.r); r.status != tc.status || r.errCode() != tc.code {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	// A server built without the outbox service does not serve the routes.
	// Every real server has it, so the contract does not list this 404.
	bare := newEnv(t, func(d *Deps) { d.Outbox = nil })
	hr := httptest.NewRequest("GET", "/api/v1/admin/outbox", nil)
	hr.Header.Set("Authorization", "Bearer "+token(t, tenant, "usr_admin", "admin", time.Hour))
	rec := httptest.NewRecorder()
	bare.handler.ServeHTTP(rec, hr)
	if rec.Code != 404 {
		t.Errorf("without the outbox service: %d", rec.Code)
	}
}

// TestOutboxPages pages through more rows than one page holds.
func TestOutboxPages(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(time.Second)
		e.createTask(nil)
	}
	r := e.do(req{method: "GET", path: "/api/v1/admin/outbox?limit=2", as: "admin"})
	if len(r.json["entries"].([]any)) != 2 || r.json["next_cursor"] == nil {
		t.Fatalf("first page: %s", r.raw)
	}
	r = e.do(req{method: "GET", path: "/api/v1/admin/outbox?limit=2&cursor=" + r.json["next_cursor"].(string), as: "admin"})
	if len(r.json["entries"].([]any)) != 1 || r.json["next_cursor"] != nil {
		t.Fatalf("second page: %s", r.raw)
	}
}
