package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/contract"
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
	r = e.do(req{method: "GET", path: "/api/v1/agent-jobs?agent_type=digest&status=pending", as: "admin"}) // no person: admins only
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

// TestAgentJobVisibility covers each kind of job against a member, the
// person a job is for, and an admin (owner decision, 2026-11-23): a job on a
// task is seen by every member, a brief by its person and admins, a job
// about the tenant by admins only. Anything else is 404 and left out of
// lists, paging included.
func TestAgentJobVisibility(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	taskID := uuid.MustParse(e.createTask(nil)["id"].(string))
	onTask := e.insertJob(&taskID, domain.AgentTriage, e.now)
	brief, err := domain.NewBriefJob(domain.BriefRecipient{TenantID: tenant, UserID: "usr_bob", WorkspaceID: "ws_core"}, e.now, e.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	learning, err := domain.NewLearningJob(domain.LearningTenant{TenantID: tenant, WorkspaceID: "ws_core"}, e.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []domain.AgentJob{brief, learning} {
		if _, err := e.b.AgentJobs.Insert(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	carol := map[string]string{"Authorization": "Bearer " + token(t, tenant, "usr_carol", "member", time.Hour)}
	type viewer struct {
		name    string
		as      string
		headers map[string]string
	}
	member, owner, admin := viewer{"member", "none", carol}, viewer{"owner", "member", nil}, viewer{"admin", "admin", nil}
	for _, tc := range []struct {
		job     string
		id      uuid.UUID
		visible map[string]bool
	}{
		{"on a task", onTask.ID, map[string]bool{"member": true, "owner": true, "admin": true}},
		{"a brief for usr_bob", brief.ID, map[string]bool{"member": false, "owner": true, "admin": true}},
		{"about the tenant", learning.ID, map[string]bool{"member": false, "owner": false, "admin": true}},
	} {
		for _, v := range []viewer{member, owner, admin} {
			r := e.do(req{method: "GET", path: "/api/v1/agent-jobs/" + tc.id.String(), as: v.as, headers: v.headers})
			if want := tc.visible[v.name]; (r.status == 200) != want || (!want && (r.status != 404 || r.errCode() != CodeNotFound)) {
				t.Errorf("GET %s as %s: %d %s", tc.job, v.name, r.status, r.raw)
			}
			if tc.visible[v.name] {
				continue
			}
			// Deciding a job the caller may not see does not reveal it either.
			for _, action := range []string{"apply", "dismiss"} {
				r := e.do(req{method: "POST", path: "/api/v1/agent-jobs/" + tc.id.String() + "/" + action, as: v.as, headers: v.headers})
				if r.status != 404 || r.errCode() != CodeNotFound {
					t.Errorf("%s %s as %s: %d %s", action, tc.job, v.name, r.status, r.raw)
				}
			}
		}
	}
	// The owner may see their brief but not decide it.
	if r := e.do(req{method: "POST", path: "/api/v1/agent-jobs/" + brief.ID.String() + "/dismiss"}); r.status != 403 {
		t.Errorf("owner dismisses their brief: %d %s", r.status, r.raw)
	}

	// Lists leave out what the caller may not see, and paging one at a time
	// ends without a trace of it.
	for _, tc := range []struct {
		v    viewer
		want []string
	}{
		{member, []string{onTask.ID.String()}},
		{owner, []string{brief.ID.String(), onTask.ID.String()}},
		{admin, []string{learning.ID.String(), brief.ID.String(), onTask.ID.String()}},
	} {
		var got []string
		path := "/api/v1/agent-jobs?limit=1"
		for {
			r := e.do(req{method: "GET", path: path, as: tc.v.as, headers: tc.v.headers})
			for _, j := range r.json["agent_jobs"].([]any) {
				got = append(got, j.(map[string]any)["id"].(string))
			}
			next, _ := r.json["next_cursor"].(string)
			if next == "" {
				break
			}
			path = "/api/v1/agent-jobs?limit=1&cursor=" + next
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s lists %v, want %v", tc.v.name, got, tc.want)
		}
	}
}

func TestListWorkspaces(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"ws_b", "ws_a", "ws_c"} {
		if r := e.do(req{method: "POST", path: "/api/v1/workspaces", as: "admin", body: map[string]any{"id": id, "name": id}}); r.status != 201 {
			t.Fatalf("create %s: %d %s", id, r.status, r.raw)
		}
	}
	// Another tenant has workspaces of its own, one with a colliding ID.
	for _, id := range []string{"ws_core", "ws_secret"} {
		if r := e.do(req{method: "POST", path: "/api/v1/workspaces", as: "other-tenant", body: map[string]any{"id": id, "name": "theirs"}}); r.status != 201 {
			t.Fatalf("create other %s: %d %s", id, r.status, r.raw)
		}
	}

	ids := func(r resp) []string {
		var out []string
		for _, w := range r.json["workspaces"].([]any) {
			out = append(out, w.(map[string]any)["id"].(string))
		}
		return out
	}
	// A plain member lists their own tenant's workspaces, in ID order.
	r := e.do(req{method: "GET", path: "/api/v1/workspaces"})
	if r.status != 200 || strings.Join(ids(r), ",") != "ws_a,ws_b,ws_c,ws_core" || r.json["next_cursor"] != nil {
		t.Fatalf("member list: %d %s", r.status, r.raw)
	}
	if got := ids(e.do(req{method: "GET", path: "/api/v1/workspaces", as: "other-tenant"})); strings.Join(got, ",") != "ws_core,ws_secret" {
		t.Errorf("other tenant sees %v", got)
	}

	// A member reads only id and name; an admin reads the full workspace.
	for _, w := range r.json["workspaces"].([]any) {
		if m := w.(map[string]any); len(m) != 2 || m["name"] == nil {
			t.Errorf("member item has %v", m)
		}
	}
	ar := e.do(req{method: "GET", path: "/api/v1/workspaces", as: "admin"})
	for _, w := range ar.json["workspaces"].([]any) {
		m := w.(map[string]any)
		if m["agent_settings"] == nil || m["created_at"] == nil || m["updated_at"] == nil {
			t.Errorf("admin item is not the full workspace: %v", m)
		}
	}
	if strings.Join(ids(ar), ",") != "ws_a,ws_b,ws_c,ws_core" {
		t.Errorf("admin list: %s", ar.raw)
	}

	// Pages.
	r = e.do(req{method: "GET", path: "/api/v1/workspaces?limit=3"})
	if strings.Join(ids(r), ",") != "ws_a,ws_b,ws_c" || r.json["next_cursor"] == nil {
		t.Fatalf("first page: %s", r.raw)
	}
	r = e.do(req{method: "GET", path: "/api/v1/workspaces?limit=3&cursor=" + r.json["next_cursor"].(string)})
	if strings.Join(ids(r), ",") != "ws_core" || r.json["next_cursor"] != nil {
		t.Fatalf("second page: %s", r.raw)
	}
	for _, bad := range []string{"limit=0", "limit=201", "limit=x", "cursor=***"} {
		if r := e.do(req{method: "GET", path: "/api/v1/workspaces?" + bad}); r.status != 400 {
			t.Errorf("%s: %d", bad, r.status)
		}
	}
	if r := e.do(req{method: "GET", path: "/api/v1/workspaces", as: "none"}); r.status != 401 {
		t.Errorf("no credentials: %d", r.status)
	}

	// Conditional polling: an unchanged list is 304; a new workspace changes
	// it; another tenant's change does not leak through the tag.
	first := e.do(req{method: "GET", path: "/api/v1/workspaces"})
	tag := first.header.Get("ETag")
	if tag == "" {
		t.Fatal("no ETag")
	}
	if r := e.do(req{method: "GET", path: "/api/v1/workspaces", headers: map[string]string{"If-None-Match": tag}}); r.status != 304 || len(r.raw) != 0 {
		t.Errorf("unchanged: %d %s", r.status, r.raw)
	}
	e.do(req{method: "POST", path: "/api/v1/workspaces", as: "other-tenant", body: map[string]any{"id": "ws_more", "name": "x"}})
	if r := e.do(req{method: "GET", path: "/api/v1/workspaces", headers: map[string]string{"If-None-Match": tag}}); r.status != 304 {
		t.Errorf("after another tenant's change: %d", r.status)
	}
	e.do(req{method: "POST", path: "/api/v1/workspaces", as: "admin", body: map[string]any{"id": "ws_d", "name": "d"}})
	if r := e.do(req{method: "GET", path: "/api/v1/workspaces", headers: map[string]string{"If-None-Match": tag}}); r.status != 200 {
		t.Errorf("after a change: %d", r.status)
	}
}

// GET /tasks/{id} answers 304 to a matching If-None-Match, and its tag
// changes with anything the detail holds, comments included.
func TestGetTaskConditional(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := task["id"].(string)
	path := "/api/v1/tasks/" + id
	get := func(tag string) resp {
		h := map[string]string{}
		if tag != "" {
			h["If-None-Match"] = tag
		}
		return e.do(req{method: "GET", path: path, headers: h})
	}

	first := get("")
	tag := first.header.Get("ETag")
	if first.status != 200 || tag == "" {
		t.Fatalf("first: %d %s", first.status, tag)
	}
	if r := get(tag); r.status != 304 || len(r.raw) != 0 || r.header.Get("ETag") != tag {
		t.Errorf("unchanged: %d %q %s", r.status, r.header.Get("ETag"), r.raw)
	}
	if r := get("W/" + tag); r.status != 304 {
		t.Errorf("weak form of the tag: %d", r.status)
	}
	if r := get(`"1.000000000000"`); r.status != 200 {
		t.Errorf("a tag that is not the task's: %d", r.status)
	}

	// A comment does not change the version, but does change the tag.
	e.do(req{method: "POST", path: path + "/comments", body: map[string]any{"body": "hello"}})
	afterComment := get(tag)
	if afterComment.status != 200 || afterComment.header.Get("ETag") == tag || len(afterComment.json["comments"].([]any)) != 1 {
		t.Fatalf("after a comment: %d %s", afterComment.status, afterComment.raw)
	}
	tag = afterComment.header.Get("ETag")
	if r := get(tag); r.status != 304 {
		t.Errorf("unchanged after the comment: %d", r.status)
	}

	// An edit changes it too, and the tag still works as If-Match.
	patched := e.do(req{method: "PATCH", path: path, headers: map[string]string{"If-Match": tag}, body: map[string]any{"title": "Renamed"}})
	if patched.status != 200 {
		t.Fatalf("If-Match with a detail tag: %d %s", patched.status, patched.raw)
	}
	if r := get(tag); r.status != 200 || r.json["title"] != "Renamed" {
		t.Errorf("after an edit: %d", r.status)
	}

	// Another tenant never learns whether the tag matches.
	if r := e.do(req{method: "GET", path: path, as: "other-tenant", headers: map[string]string{"If-None-Match": tag}}); r.status != 404 {
		t.Errorf("other tenant: %d", r.status)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks/" + uuid.NewString(), headers: map[string]string{"If-None-Match": tag}}); r.status != 404 {
		t.Errorf("unknown task: %d", r.status)
	}
}

// Every response carries the contract's version, in one place: the embedded
// openapi.yaml.
func TestAPIVersionHeader(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.CORSAllowedOrigins = []string{"https://app.example"} })
	want := contract.APIVersion()
	if want == "" {
		t.Fatal("no contract version")
	}
	for name, rq := range map[string]req{
		"ok":            {method: "GET", path: "/api/v1/me"},
		"not found":     {method: "GET", path: "/api/v1/tasks/" + uuid.NewString()},
		"unauthorized":  {method: "GET", path: "/api/v1/me", as: "none"},
		"no such route": {method: "GET", path: "/api/v1/nope"},
		"bad method":    {method: "DELETE", path: "/api/v1/me"},
		"healthz":       {method: "GET", path: "/healthz", as: "none"},
		"readyz":        {method: "GET", path: "/readyz", as: "none"},
		"not modified":  {method: "GET", path: "/api/v1/workspaces", headers: map[string]string{"If-None-Match": "*"}},
	} {
		if r := e.do(rq); r.header.Get("X-Taskify-API-Version") != want {
			t.Errorf("%s (%d): X-Taskify-API-Version = %q, want %q", name, r.status, r.header.Get("X-Taskify-API-Version"), want)
		}
	}
	// A preflight is not in the contract, so it goes straight to the handler.
	pre := httptest.NewRequest("OPTIONS", "/api/v1/tasks", nil)
	pre.Header.Set("Origin", "https://app.example")
	pre.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, pre)
	if rec.Code != 204 || rec.Header().Get("X-Taskify-API-Version") != want {
		t.Errorf("preflight: %d %q", rec.Code, rec.Header().Get("X-Taskify-API-Version"))
	}
	// A browser on another origin may read it.
	r := e.do(req{method: "GET", path: "/api/v1/me", headers: map[string]string{"Origin": "https://app.example"}})
	if !strings.Contains(r.header.Get("Access-Control-Expose-Headers"), "X-Taskify-API-Version") {
		t.Errorf("not exposed to browsers: %q", r.header.Get("Access-Control-Expose-Headers"))
	}
}

func TestAdminAgentJobRuns(t *testing.T) {
	e := newEnv(t)
	job := e.insertJob(nil, domain.AgentDigest, e.now)
	run := domain.AgentRun{ID: uuid.New(), JobID: job.ID, Turn: 1, Model: "m", Outcome: "final",
		PromptTokens: 7, CompletionTokens: 3, CreatedAt: e.now}
	if err := e.b.AgentJobs.InsertRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/agent-jobs/" + job.ID.String() + "/runs"
	r := e.do(req{method: "GET", path: path, as: "admin"})
	runs, _ := r.json["runs"].([]any)
	if r.status != 200 || len(runs) != 1 || r.json["job"].(map[string]any)["id"] != job.ID.String() ||
		runs[0].(map[string]any)["prompt_tokens"] != 7.0 {
		t.Fatalf("runs: %d %s", r.status, r.raw)
	}
	for name, rq := range map[string]req{
		"member":       {method: "GET", path: path},
		"other tenant": {method: "GET", path: path, as: "other-tenant"},
		"unknown":      {method: "GET", path: "/api/v1/admin/agent-jobs/" + uuid.NewString() + "/runs", as: "admin"},
		"malformed":    {method: "GET", path: "/api/v1/admin/agent-jobs/nope/runs", as: "admin"},
	} {
		want := 404
		if name == "member" {
			want = 403
		}
		if r := e.do(rq); r.status != want {
			t.Errorf("%s: %d", name, r.status)
		}
	}
}
