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
