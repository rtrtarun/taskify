package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTaskListETag is M14-02: an unchanged list answers 304, and anything
// that changes it answers a new tag.
func TestTaskListETag(t *testing.T) {
	e := newEnv(t)
	a := e.createTask(nil)
	first := e.do(req{method: "GET", path: "/api/v1/tasks"})
	tag := first.header.Get("ETag")
	if first.status != 200 || tag == "" || tag[:3] != `W/"` {
		t.Fatalf("first list: %d etag %q", first.status, tag)
	}

	again := e.do(req{method: "GET", path: "/api/v1/tasks", headers: map[string]string{"If-None-Match": tag}})
	if again.status != http.StatusNotModified || len(again.raw) != 0 || again.header.Get("ETag") != tag {
		t.Fatalf("unchanged list: %d %q etag %q", again.status, again.raw, again.header.Get("ETag"))
	}
	for _, h := range []string{"*", `"x", ` + tag, tag[2:]} { // a list, a wildcard, and the strong form
		if r := e.do(req{method: "GET", path: "/api/v1/tasks", headers: map[string]string{"If-None-Match": h}}); r.status != 304 {
			t.Errorf("If-None-Match %q: %d", h, r.status)
		}
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks", headers: map[string]string{"If-None-Match": `W/"nope"`}}); r.status != 200 {
		t.Errorf("stale tag: %d", r.status)
	}

	// Another query or another tenant has its own tag.
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?status=todo"}); r.header.Get("ETag") == tag {
		t.Error("a different filter shares the tag")
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks", as: "other-tenant"}); r.header.Get("ETag") == tag {
		t.Error("another tenant shares the tag")
	}

	// An edit moves the latest update, so the tag changes.
	e.now = e.now.Add(time.Minute)
	id := a["id"].(string)
	if r := e.do(req{method: "PATCH", path: "/api/v1/tasks/" + id, headers: map[string]string{"If-Match": `"1"`}, body: map[string]any{"priority": "p2_high"}}); r.status != 200 {
		t.Fatalf("edit: %d %s", r.status, r.raw)
	}
	edited := e.do(req{method: "GET", path: "/api/v1/tasks", headers: map[string]string{"If-None-Match": tag}})
	if edited.status != 200 || edited.header.Get("ETag") == tag {
		t.Fatalf("after an edit: %d etag %q", edited.status, edited.header.Get("ETag"))
	}
	tag = edited.header.Get("ETag")

	// A new task changes the count.
	e.createTask(nil)
	if r := e.do(req{method: "GET", path: "/api/v1/tasks", headers: map[string]string{"If-None-Match": tag}}); r.status != 200 {
		t.Errorf("after a create: %d", r.status)
	}
}

// TestTaskListDelta covers updated_since and archived_ids.
func TestTaskListDelta(t *testing.T) {
	e := newEnv(t)
	old := e.createTask(map[string]any{"title": "Old"})
	keep := e.createTask(map[string]any{"title": "Kept"})
	idle := e.createTask(map[string]any{"title": "Idle"})
	since := e.now.Add(30 * time.Second)
	sinceQ := "updated_since=" + since.Format(time.RFC3339)

	e.now = e.now.Add(time.Minute)
	archiveID, restoreID := old["id"].(string), keep["id"].(string)
	if r := e.do(req{method: "DELETE", path: "/api/v1/tasks/" + archiveID, headers: map[string]string{"If-Match": `"1"`}}); r.status != 200 && r.status != 204 {
		t.Fatalf("archive: %d %s", r.status, r.raw)
	}
	// An archived and restored task is a task, not an archived ID.
	if r := e.do(req{method: "DELETE", path: "/api/v1/tasks/" + restoreID, headers: map[string]string{"If-Match": `"1"`}}); r.status != 200 && r.status != 204 {
		t.Fatalf("archive 2: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/tasks/" + restoreID + "/restore"}); r.status != 200 {
		t.Fatalf("restore: %d %s", r.status, r.raw)
	}
	fresh := e.createTask(map[string]any{"title": "New"})

	r := e.do(req{method: "GET", path: "/api/v1/tasks?" + sinceQ})
	tasks, _ := r.json["tasks"].([]any)
	archived, _ := r.json["archived_ids"].([]any)
	if r.status != 200 || len(tasks) != 2 || len(archived) != 1 || archived[0] != archiveID {
		t.Fatalf("delta: %d %s", r.status, r.raw)
	}
	ids := map[any]bool{}
	for _, tk := range tasks {
		ids[tk.(map[string]any)["id"]] = true
	}
	if !ids[restoreID] || !ids[fresh["id"]] {
		t.Errorf("delta tasks: %s", r.raw)
	}

	// Without updated_since there is no archived_ids; a bad time is refused.
	if r := e.do(req{method: "GET", path: "/api/v1/tasks"}); r.json["archived_ids"] != nil {
		t.Errorf("archived_ids without updated_since: %s", r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?updated_since=yesterday"}); r.status != 400 {
		t.Errorf("bad updated_since: %d", r.status)
	}
	// Filters apply to archived_ids too.
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?priority=p1_critical&" + sinceQ}); len(r.json["archived_ids"].([]any)) != 0 {
		t.Errorf("filtered archived_ids: %s", r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?" + sinceQ, as: "other-tenant"}); len(r.json["archived_ids"].([]any)) != 0 {
		t.Errorf("another tenant's archived_ids: %s", r.raw)
	}

	// Archiving a task that was not in the delta (idle, untouched since before
	// updated_since) changes neither the count nor the latest update of what
	// remains, yet the delta now reports it, so the tag must change.
	dtag := r.header.Get("ETag")
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?" + sinceQ, headers: map[string]string{"If-None-Match": dtag}}); r.status != 304 {
		t.Fatalf("unchanged delta: %d", r.status)
	}
	e.now = e.now.Add(time.Minute)
	e.do(req{method: "DELETE", path: "/api/v1/tasks/" + idle["id"].(string), headers: map[string]string{"If-Match": `"1"`}})
	after := e.do(req{method: "GET", path: "/api/v1/tasks?" + sinceQ, headers: map[string]string{"If-None-Match": dtag}})
	if after.status != 200 || len(after.json["archived_ids"].([]any)) != 2 {
		t.Errorf("delta after the archive: %d %s", after.status, after.raw)
	}
}

// TestOtherListsConditional: agent jobs and outbox answer 304 for a repeated
// poll.
func TestOtherListsConditional(t *testing.T) {
	e := newEnv(t)
	e.createTask(nil)
	for _, c := range []struct{ path, as string }{{"/api/v1/agent-jobs", ""}, {"/api/v1/admin/outbox", "admin"}} {
		first := e.do(req{method: "GET", path: c.path, as: c.as})
		tag := first.header.Get("ETag")
		if first.status != 200 || tag == "" {
			t.Fatalf("%s: %d etag %q", c.path, first.status, tag)
		}
		if r := e.do(req{method: "GET", path: c.path, as: c.as, headers: map[string]string{"If-None-Match": tag}}); r.status != 304 || len(r.raw) != 0 {
			t.Errorf("%s unchanged: %d", c.path, r.status)
		}
	}
	e.createTask(nil) // writes outbox rows
	first := e.do(req{method: "GET", path: "/api/v1/admin/outbox", as: "admin"})
	if first.header.Get("ETag") == "" {
		t.Fatal("no tag")
	}
}

func corsRequest(h http.Handler, method, origin string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/tasks", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestCORS(t *testing.T) {
	const allowed = "https://app.example.com"
	e := newEnv(t, func(d *Deps) { d.CORSAllowedOrigins = []string{allowed} })
	preflight := map[string]string{"Access-Control-Request-Method": "PATCH", "Access-Control-Request-Headers": "authorization, if-match, x-evil"}

	rec := corsRequest(e.handler, http.MethodOptions, allowed, preflight)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != allowed ||
		rec.Header().Get("Access-Control-Allow-Headers") != "Authorization, If-Match" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("allowed preflight: %d %v", rec.Code, rec.Header())
	}

	rec = corsRequest(e.handler, http.MethodOptions, "https://evil.example", preflight)
	if rec.Code != 403 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disallowed preflight: %d %v", rec.Code, rec.Header())
	}

	// An actual request: exposed headers for the allowed origin, none for others.
	rec = corsRequest(e.handler, http.MethodGet, allowed, nil)
	if rec.Header().Get("Access-Control-Allow-Origin") != allowed || rec.Header().Get("Access-Control-Expose-Headers") == "" || rec.Code != 401 {
		t.Errorf("allowed request: %d %v", rec.Code, rec.Header())
	}
	rec = corsRequest(e.handler, http.MethodGet, "https://evil.example", nil)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Expose-Headers") != "" {
		t.Errorf("disallowed request got CORS headers: %v", rec.Header())
	}
	rec = corsRequest(e.handler, http.MethodGet, "", nil) // curl, the CLI
	if rec.Header().Get("Vary") != "" || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("no origin: %v", rec.Header())
	}

	// With no list configured the middleware is out of the way.
	plain := newEnv(t)
	rec = corsRequest(plain.handler, http.MethodOptions, allowed, preflight)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Code == 204 {
		t.Errorf("no allow list: %d %v", rec.Code, rec.Header())
	}
}
