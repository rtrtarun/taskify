package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

// brief stores a completed brief for user in tenantID on day, with text,
// and returns its job ID. A brief with no text is one skipped for having
// nothing to report.
func (e *env) brief(tenantID, user string, day time.Time, text string) string {
	e.t.Helper()
	ctx := context.Background()
	job, err := domain.NewBriefJob(domain.BriefRecipient{TenantID: tenantID, UserID: user, WorkspaceID: "ws_core"}, day, day)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.b.AgentJobs.Insert(ctx, job); err != nil {
		e.t.Fatal(err)
	}
	result := map[string]any{"turns": 0, "stopped": "skipped", "skip_reason": "nothing to report"}
	if text != "" {
		taskID := "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
		result = map[string]any{"turns": 1, "stopped": "final", "report": map[string]any{
			"brief_date": day.Format(time.DateOnly), "user_id": user, "text": text, "open_tasks": 4,
			"top_items": []map[string]any{{"task_id": taskID, "title": "Fix checkout", "workspace_id": "ws_core",
				"status": "todo", "priority": "p1_critical", "due_at": nil, "overdue": false}},
			"pending_approvals": []any{}, "pending_approvals_total": 0, "unblocked_tasks": []any{}, "unblocked_total": 0,
		}}
	}
	job.ExecutionResult, _ = json.Marshal(result)
	prev := job.UpdatedAt
	job.Status, job.UpdatedAt = domain.JobCompleted, day.Add(time.Minute)
	if err := e.b.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
		e.t.Fatal(err)
	}
	return job.ID.String()
}

// TestMyBrief is the M10-03 test: a user sees only their own brief, the
// most recent one written, with tenant and user taken from the token.
func TestMyBrief(t *testing.T) {
	e := newEnv(t)
	day := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	e.brief(tenant, "usr_bob", day, "Monday's brief")
	bobs := e.brief(tenant, "usr_bob", day.Add(24*time.Hour), "Tuesday's brief")
	e.brief(tenant, "usr_bob", day.Add(48*time.Hour), "") // nothing to report on Wednesday
	carols := e.brief(tenant, "usr_carol", day.Add(48*time.Hour), "Carol's brief")
	theirs := e.brief("tenant_other", "usr_bob", day.Add(48*time.Hour), "Another tenant's bob")

	r := e.do(req{method: "GET", path: "/api/v1/me/brief"}) // as usr_bob
	if r.status != 200 || r.json["job_id"] != bobs || r.json["text"] != "Tuesday's brief" || r.json["user_id"] != "usr_bob" ||
		r.json["brief_date"] != "2026-11-21" || r.json["written_at"] != "2026-11-21T08:01:00Z" || r.json["open_tasks"] != float64(4) {
		t.Fatalf("bob's brief: %d %s", r.status, r.raw)
	}
	items, _ := r.json["top_items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["title"] != "Fix checkout" {
		t.Errorf("top_items = %v", r.json["top_items"])
	}
	for _, k := range []string{"pending_approvals", "pending_approvals_total", "unblocked_tasks", "unblocked_total"} {
		if _, ok := r.json[k]; !ok {
			t.Errorf("response is missing %q: %s", k, r.raw)
		}
	}
	if _, leaked := r.json["TenantID"]; leaked {
		t.Errorf("response carries the tenant: %s", r.raw)
	}

	as := func(tenantID, user string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + token(t, tenantID, user, "member", time.Hour)}
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief", headers: as(tenant, "usr_carol")}); r.status != 200 || r.json["job_id"] != carols {
		t.Errorf("carol's brief: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief", headers: as("tenant_other", "usr_bob")}); r.status != 200 || r.json["job_id"] != theirs {
		t.Errorf("bob in another tenant: %d %s", r.status, r.raw)
	}
	// An admin reads their own brief too, not anyone else's.
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief", as: "admin"}); r.status != 404 || r.errCode() != CodeNotFound {
		t.Errorf("admin without a brief: %d %s", r.status, r.raw)
	}
	// The path takes no user: a query naming someone else changes nothing.
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief?user_id=usr_carol"}); r.json["job_id"] != bobs {
		t.Errorf("query parameter: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief", headers: as(tenant, "usr_dave")}); r.status != 404 || r.errCode() != CodeNotFound {
		t.Errorf("no brief yet: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief", as: "none"}); r.status != 401 {
		t.Errorf("no token: %d", r.status)
	}
}

// TestMyBriefAfterPermanentDelete is AP-08 through HTTP: once a task is
// permanently deleted, GET /me/brief returns an older brief that does not
// name it, or 404.
func TestMyBriefAfterPermanentDelete(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(map[string]any{"title": "Acquire Initech quietly"})
	id := task["id"].(string)
	day := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	older := e.brief(tenant, "usr_bob", day, "Monday's brief") // names another task
	named := func(user string, day time.Time) {
		job, _ := domain.NewBriefJob(domain.BriefRecipient{TenantID: tenant, UserID: user, WorkspaceID: "ws_core"}, day, day)
		if _, err := e.b.AgentJobs.Insert(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		job.ExecutionResult, _ = json.Marshal(map[string]any{"turns": 1, "stopped": "final", "report": map[string]any{
			"brief_date": day.Format(time.DateOnly), "user_id": user, "text": "Acquire Initech quietly first",
			"top_items": []map[string]any{{"task_id": id, "title": "Acquire Initech quietly", "workspace_id": "ws_core",
				"status": "todo", "priority": "p3_medium", "due_at": nil, "overdue": false}},
			"pending_approvals": []any{}, "unblocked_tasks": []any{}}})
		prev := job.UpdatedAt
		job.Status, job.UpdatedAt = domain.JobCompleted, day.Add(time.Minute)
		if err := e.b.AgentJobs.Update(context.Background(), &job, domain.JobPending, prev); err != nil {
			t.Fatal(err)
		}
	}
	named("usr_bob", day.Add(24*time.Hour))
	named("usr_carol", day.Add(24*time.Hour))
	carol := map[string]string{"Authorization": "Bearer " + token(t, tenant, "usr_carol", "member", time.Hour)}
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief"}); r.json["text"] != "Acquire Initech quietly first" {
		t.Fatalf("before the delete: %s", r.raw)
	}

	if r := e.do(req{method: "DELETE", path: "/api/v1/tasks/" + id + "?permanent=true", as: "admin"}); r.status != 204 {
		t.Fatalf("delete: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief"}); r.status != 200 || r.json["job_id"] != older {
		t.Errorf("bob after the delete: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/me/brief", headers: carol}); r.status != 404 {
		t.Errorf("carol after the delete: %d %s", r.status, r.raw)
	}
}
