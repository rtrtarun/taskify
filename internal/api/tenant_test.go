package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestTenantIDsAtEveryEntryPoint is AP-04: the JWT claim and the development
// headers accept only AgentPulse's tenant pattern, exactly as given, and
// refuse "default".
func TestTenantIDsAtEveryEntryPoint(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Auth.Development = true })
	valid := []string{tenant, "tenant_dev", "Acme.Corp-1", "a", "Default", strings.Repeat("t", 64)}
	invalid := []string{"default", " tenant_acme", "tenant_acme ", "tenant acme", "-acme", "_acme", ".acme", "acme/1",
		"ténant", strings.Repeat("t", 65)}

	viaToken := func(tenantID string) resp {
		return e.do(req{method: "GET", path: "/api/v1/tasks", as: "none",
			headers: map[string]string{"Authorization": "Bearer " + token(t, tenantID, "usr_bob", "member", time.Hour)}})
	}
	viaHeaders := func(tenantID string) resp {
		return e.do(req{method: "GET", path: "/api/v1/tasks", as: "none",
			headers: map[string]string{HeaderTenantID: tenantID, HeaderUserID: "usr_bob"}})
	}
	for name, enter := range map[string]func(string) resp{"token": viaToken, "headers": viaHeaders} {
		for _, id := range valid {
			if r := enter(id); r.status != http.StatusOK {
				t.Errorf("%s, tenant %q: %d %s", name, id, r.status, r.raw)
			}
		}
		for _, id := range invalid {
			r := enter(id)
			if r.status != http.StatusUnauthorized || r.errCode() != CodeUnauthorized || !strings.Contains(string(r.raw), "tenant ID matching") {
				t.Errorf("%s, tenant %q: %d %s", name, id, r.status, r.raw)
			}
		}
	}

	// Case matters: TENANT_ACME is another tenant and sees none of
	// tenant_acme's tasks.
	task := e.createTask(nil)
	upper := e.do(req{method: "GET", path: "/api/v1/tasks/" + task["id"].(string), as: "none",
		headers: map[string]string{"Authorization": "Bearer " + token(t, strings.ToUpper(tenant), "usr_bob", "admin", time.Hour)}})
	if upper.status != http.StatusNotFound {
		t.Errorf("another tenant by case: %d %s", upper.status, upper.raw)
	}

	// A webhook callback naming a tenant Taskify cannot have is refused.
	for _, id := range []string{"default", " " + tenant, "tenant acme"} {
		body := map[string]any{"action": "complete", "tenantId": id, "userId": "usr_carol", "taskId": task["id"]}
		if r := e.webhook(body, e.now.Unix(), ""); r.status != http.StatusUnprocessableEntity {
			t.Errorf("webhook tenant %q: %d %s", id, r.status, r.raw)
		}
	}
	if got := e.do(req{method: "GET", path: "/api/v1/tasks/" + task["id"].(string)}); got.json["status"] != "todo" {
		t.Errorf("a refused callback changed the task: %v", got.json["status"])
	}
}

// TestWebhookCallbackID is AP-06 through HTTP: a retry of one click, signed
// again with a new timestamp, is a duplicate and forwards no second
// feedback; a byte-identical replay is still a duplicate.
func TestWebhookCallbackID(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	body := map[string]any{"action": "complete", "tenantId": tenant, "userId": "usr_carol", "taskId": task["id"],
		"notificationId": "ntf_1", "callbackId": "cb-0123456789abcdef0123456789abcdef"}
	e.mock.SeedNotification("ntf_1", map[string]any{"tenantId": tenant, "userId": "usr_carol"})
	now := e.now.Unix()
	if r := e.webhook(body, now, ""); r.status != http.StatusOK || r.json["status"] != "applied" {
		t.Fatalf("first delivery: %d %s", r.status, r.raw)
	}
	if r := e.webhook(body, now, ""); r.status != http.StatusOK || r.json["status"] != "duplicate" {
		t.Errorf("byte-identical replay: %d %s", r.status, r.raw)
	}
	if r := e.webhook(body, now+30, ""); r.status != http.StatusOK || r.json["status"] != "duplicate" {
		t.Errorf("retry signed again: %d %s", r.status, r.raw)
	}
	if fb := e.mock.Feedback(); len(fb) != 1 {
		t.Errorf("feedback = %v, want one signal", fb)
	}
	body["callbackId"] = "not-a-callback-id"
	if r := e.webhook(body, now, ""); r.status != http.StatusUnprocessableEntity {
		t.Errorf("malformed callbackId: %d %s", r.status, r.raw)
	}
}

// TestUserIDsAtEveryEntryPoint is AP-11: the JWT subject and the
// development headers accept only user IDs of 1 to 128 printable ASCII
// characters, after trimming, and a webhook callback naming another user
// ID is refused with 422.
func TestUserIDsAtEveryEntryPoint(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Auth.Development = true })
	valid := []string{"usr_bob", "a", "alice@example.com", "Bob Smith", "user:42", strings.Repeat("u", 128)}
	invalid := []string{"josé", "用户", "usr\tbob", "usr\x7fbob", strings.Repeat("u", 129)}

	viaToken := func(user string) resp {
		return e.do(req{method: "GET", path: "/api/v1/tasks", as: "none",
			headers: map[string]string{"Authorization": "Bearer " + token(t, tenant, user, "member", time.Hour)}})
	}
	viaHeaders := func(user string) resp {
		return e.do(req{method: "GET", path: "/api/v1/tasks", as: "none",
			headers: map[string]string{HeaderTenantID: tenant, HeaderUserID: user}})
	}
	for name, enter := range map[string]func(string) resp{"token": viaToken, "headers": viaHeaders} {
		for _, id := range valid {
			if r := enter(id); r.status != http.StatusOK {
				t.Errorf("%s, user %q: %d %s", name, id, r.status, r.raw)
			}
		}
		for _, id := range invalid {
			r := enter(id)
			if r.status != http.StatusUnauthorized || !strings.Contains(string(r.raw), "printable ASCII") {
				t.Errorf("%s, user %q: %d %s", name, id, r.status, r.raw)
			}
		}
	}
	// The subject is trimmed, as before.
	if r := viaToken(" usr_bob "); r.status != http.StatusOK {
		t.Errorf("padded subject: %d %s", r.status, r.raw)
	}

	// Task fields that name people follow the rule too.
	if r := e.do(req{method: "POST", path: "/api/v1/tasks", body: map[string]any{"workspace_id": "ws_core", "title": "x",
		"assignee_id": "josé"}}); r.status != http.StatusUnprocessableEntity || !strings.Contains(string(r.raw), "assignee_id must be") {
		t.Errorf("assignee josé: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/tasks", body: map[string]any{"workspace_id": "ws_core", "title": "x",
		"watcher_ids": []string{"usr_carol", "zoë"}}}); r.status != http.StatusUnprocessableEntity || !strings.Contains(string(r.raw), "watcher ID must be") {
		t.Errorf("watcher zoë: %d %s", r.status, r.raw)
	}

	// A webhook callback naming a user Taskify cannot have is refused.
	task := e.createTask(nil)
	for _, id := range []string{"josé", "usr\x01"} {
		body := map[string]any{"action": "complete", "tenantId": tenant, "userId": id, "taskId": task["id"]}
		if r := e.webhook(body, e.now.Unix(), ""); r.status != http.StatusUnprocessableEntity {
			t.Errorf("webhook user %q: %d %s", id, r.status, r.raw)
		}
	}
	if got := e.do(req{method: "GET", path: "/api/v1/tasks/" + task["id"].(string)}); got.json["status"] != "todo" {
		t.Errorf("a refused callback changed the task: %v", got.json["status"])
	}
}
