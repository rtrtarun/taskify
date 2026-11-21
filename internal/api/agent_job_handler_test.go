package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/agenttest"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
)

// proposal stores a job awaiting approval of one Describe call on the task,
// based on its current version.
func (e *env) proposal(taskID, description string) string {
	e.t.Helper()
	ctx := context.Background()
	id := uuid.MustParse(taskID)
	task, err := e.b.Tasks.Get(ctx, tenant, id)
	if err != nil {
		e.t.Fatal(err)
	}
	job, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, TaskID: &id, AgentType: domain.AgentTriage,
		DedupKey: "test:" + uuid.NewString(), BaseTaskVersion: &task.Version}, e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.b.AgentJobs.Insert(ctx, job); err != nil {
		e.t.Fatal(err)
	}
	args, _ := json.Marshal(agenttest.DescribeArgs{TaskID: id, Description: description})
	job.ProposedActions, _ = json.Marshal([]tools.Proposal{{Tool: agenttest.DescribeToolName, Arguments: args, CallID: "c1"}})
	prev := job.UpdatedAt
	job.Status = domain.JobAwaitingApproval
	if err := e.b.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
		e.t.Fatal(err)
	}
	return job.ID.String()
}

func TestAgentJobEndpoints(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil) // created by usr_bob, who may decide
	taskID := task["id"].(string)
	jobID := e.proposal(taskID, "Written by the agent")
	path := "/api/v1/agent-jobs/" + jobID

	r := e.do(req{method: "GET", path: path})
	if r.status != 200 || r.json["status"] != "awaiting_approval" || r.json["task_id"] != taskID || r.json["agent_type"] != "triage" {
		t.Fatalf("GET: %d %s", r.status, r.raw)
	}
	if props, _ := r.json["proposed_actions"].([]any); len(props) != 1 || r.json["decided_by"] != nil {
		t.Errorf("GET body = %s", r.raw)
	}

	for name, rq := range map[string]req{
		"unknown":      {method: "GET", path: "/api/v1/agent-jobs/" + uuid.NewString()},
		"malformed":    {method: "GET", path: "/api/v1/agent-jobs/not-a-uuid"},
		"other tenant": {method: "GET", path: path, as: "other-tenant"},
		"apply other":  {method: "POST", path: path + "/apply", as: "other-tenant"},
	} {
		if r := e.do(rq); r.status != 404 || r.errCode() != CodeNotFound {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	if r := e.do(req{method: "GET", path: path, as: "none"}); r.status != 401 {
		t.Errorf("no token: %d", r.status)
	}

	// Someone who is neither assignee, creator, nor admin.
	stranger := map[string]string{"Authorization": "Bearer " + token(t, tenant, "usr_eve", "member", time.Hour)}
	if r := e.do(req{method: "POST", path: path + "/apply", headers: stranger}); r.status != 403 || r.errCode() != CodeForbidden {
		t.Errorf("stranger apply: %d %s", r.status, r.raw)
	}

	r = e.do(req{method: "POST", path: path + "/apply"})
	job, _ := r.json["job"].(map[string]any)
	if r.status != 200 || r.json["changed"] != true || job["status"] != "completed" || job["decided_by"] != "usr_bob" {
		t.Fatalf("apply: %d %s", r.status, r.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/tasks/" + taskID}); got.json["description"] != "Written by the agent" {
		t.Errorf("task after apply = %s", got.raw)
	}
	if r := e.do(req{method: "POST", path: path + "/apply"}); r.status != 200 || r.json["changed"] != false {
		t.Errorf("second apply: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "POST", path: path + "/dismiss"}); r.status != 409 || r.errCode() != CodeAlreadyDecided {
		t.Errorf("dismiss after apply: %d %s", r.status, r.raw)
	}
}

func TestAgentJobStaleAndPending(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	taskID := task["id"].(string)
	jobID := e.proposal(taskID, "too late")

	// A person edits the task first.
	upd := e.do(req{method: "PATCH", path: "/api/v1/tasks/" + taskID, body: map[string]any{"title": "Renamed"},
		headers: map[string]string{"If-Match": `"1"`}})
	if upd.status != 200 {
		t.Fatalf("edit: %d %s", upd.status, upd.raw)
	}
	r := e.do(req{method: "POST", path: "/api/v1/agent-jobs/" + jobID + "/apply"})
	if r.status != http.StatusConflict || r.errCode() != CodeStaleVersion {
		t.Errorf("stale apply: %d %s", r.status, r.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/agent-jobs/" + jobID}); got.json["status"] != "failed" || got.json["error_message"] != "stale_version" {
		t.Errorf("job after stale apply = %s", got.raw)
	}

	// A job that has not asked yet.
	id := uuid.MustParse(taskID)
	pending, _ := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, TaskID: &id, AgentType: domain.AgentTriage,
		DedupKey: "pending:" + uuid.NewString()}, e.now)
	if _, err := e.b.AgentJobs.Insert(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/agent-jobs/" + pending.ID.String() + "/dismiss", as: "admin"}); r.status != 409 || r.errCode() != CodeNotAwaitingApproval {
		t.Errorf("dismiss pending: %d %s", r.status, r.raw)
	}
}

func TestWebhookApproveAndReject(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil) // assignee usr_carol, creator usr_bob
	taskID := task["id"].(string)
	now := e.now.Unix()
	feedbackFor := func(signal string) int {
		n := 0
		for _, fb := range e.mock.Feedback() {
			if fb["signal"] == signal && fb["taskId"] == taskID {
				n++
			}
		}
		return n
	}

	// Approve, from the assignee's notification.
	jobID := e.proposal(taskID, "Approved in Slack")
	approve := map[string]any{"action": "approve", "tenantId": tenant, "userId": "usr_carol", "jobId": jobID,
		"taskId": taskID, "notificationId": "ntf_approve"}
	if r := e.webhook(approve, now, ""); r.status != 200 || r.json["status"] != "applied" {
		t.Fatalf("approve: %d %s", r.status, r.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/tasks/" + taskID}); got.json["description"] != "Approved in Slack" {
		t.Errorf("task after approve = %s", got.raw)
	}
	if n := feedbackFor("acted"); n != 1 {
		t.Errorf("acted signals = %d, want 1", n)
	}
	// A replay, and a fresh delivery of the same decision: no second signal.
	if r := e.webhook(approve, now, ""); r.status != 200 || r.json["status"] != "duplicate" {
		t.Errorf("replay: %d %s", r.status, r.raw)
	}
	if r := e.webhook(approve, now+1, ""); r.status != 200 || r.json["status"] != "duplicate" {
		t.Errorf("repeat: %d %s", r.status, r.raw)
	}
	if n := feedbackFor("acted"); n != 1 {
		t.Errorf("acted signals after repeats = %d, want 1", n)
	}
	// The opposite decision on a decided job is refused.
	reject := map[string]any{"action": "reject", "tenantId": tenant, "userId": "usr_carol", "jobId": jobID}
	if r := e.webhook(reject, now+2, ""); r.status != 409 || r.errCode() != CodeAlreadyDecided {
		t.Errorf("reject after approve: %d %s", r.status, r.raw)
	}

	// Reject a second proposal.
	second := e.proposal(taskID, "never")
	reject["jobId"] = second
	if r := e.webhook(reject, now+3, ""); r.status != 200 || r.json["status"] != "applied" {
		t.Fatalf("reject: %d %s", r.status, r.raw)
	}
	if n := feedbackFor("dismissed"); n != 1 {
		t.Errorf("dismissed signals = %d, want 1", n)
	}

	// The callback's user goes through the same permission check.
	third := e.proposal(taskID, "not yours")
	stranger := map[string]any{"action": "approve", "tenantId": tenant, "userId": "usr_eve", "jobId": third}
	if r := e.webhook(stranger, now+4, ""); r.status != 403 || r.errCode() != CodeForbidden {
		t.Errorf("stranger approve: %d %s", r.status, r.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/agent-jobs/" + third}); got.json["status"] != "awaiting_approval" {
		t.Errorf("job after a refused callback = %s", got.raw)
	}

	// Malformed or missing job IDs.
	if r := e.webhook(map[string]any{"action": "approve", "tenantId": tenant, "userId": "usr_carol", "jobId": "x"}, now+5, ""); r.status != 404 {
		t.Errorf("malformed jobId: %d %s", r.status, r.raw)
	}
	if r := e.webhook(map[string]any{"action": "approve", "tenantId": tenant, "userId": "usr_carol"}, now+6, ""); r.status != 422 {
		t.Errorf("missing jobId: %d %s", r.status, r.raw)
	}
}

func TestDecomposeEndpoint(t *testing.T) {
	e := newEnv(t)
	taskID := e.createTask(nil)["id"].(string)
	path := "/api/v1/tasks/" + taskID + "/decompose"
	settings := func(s map[string]any) {
		t.Helper()
		if r := e.do(req{method: "PATCH", path: "/api/v1/workspaces/ws_core/settings", as: "admin",
			body: map[string]any{"agent_settings": s}}); r.status != 200 {
			t.Fatalf("settings: %d %s", r.status, r.raw)
		}
	}

	// G21: a disabled decomposer is 409 agent_disabled, not 202.
	if r := e.do(req{method: "POST", path: path}); r.status != 409 || r.errCode() != CodeAgentDisabled {
		t.Errorf("agents off: %d %s", r.status, r.raw)
	}
	settings(map[string]any{"agents_enabled": true, "disabled_agents": []string{"decomposer"}})
	if r := e.do(req{method: "POST", path: path}); r.status != 409 || r.errCode() != CodeAgentDisabled {
		t.Errorf("decomposer off: %d %s", r.status, r.raw)
	}

	settings(map[string]any{"disabled_agents": []string{}})
	r := e.do(req{method: "POST", path: path})
	if r.status != 202 || r.json["agent_type"] != "decomposer" || r.json["status"] != "pending" || r.json["task_id"] != taskID {
		t.Fatalf("decompose: %d %s", r.status, r.raw)
	}
	jobID := r.json["id"].(string)
	if loc := r.header.Get("Location"); loc != "/api/v1/agent-jobs/"+jobID {
		t.Errorf("Location = %q", loc)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/agent-jobs/" + jobID}); got.status != 200 || got.json["status"] != "pending" {
		t.Errorf("job: %d %s", got.status, got.raw)
	}
	// A repeat returns the open job.
	if again := e.do(req{method: "POST", path: path}); again.status != 202 || again.json["id"] != jobID {
		t.Errorf("repeat: %d %s", again.status, again.raw)
	}

	for name, rq := range map[string]req{
		"unknown":      {method: "POST", path: "/api/v1/tasks/" + uuid.NewString() + "/decompose"},
		"malformed":    {method: "POST", path: "/api/v1/tasks/nope/decompose"},
		"other tenant": {method: "POST", path: path, as: "other-tenant"},
	} {
		if r := e.do(rq); r.status != 404 || r.errCode() != CodeNotFound {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	done := e.createTask(nil)["id"].(string)
	if r := e.do(req{method: "POST", path: "/api/v1/tasks/" + done + "/complete"}); r.status != 200 {
		t.Fatalf("complete: %d", r.status)
	}
	archived := e.createTask(nil)["id"].(string)
	if r := e.do(req{method: "DELETE", path: "/api/v1/tasks/" + archived}); r.status != 200 {
		t.Fatalf("archive: %d", r.status)
	}
	for name, id := range map[string]string{"completed": done, "archived": archived} {
		if r := e.do(req{method: "POST", path: "/api/v1/tasks/" + id + "/decompose"}); r.status != 422 || r.errCode() != CodeValidation {
			t.Errorf("%s task: %d %s", name, r.status, r.raw)
		}
	}
}
