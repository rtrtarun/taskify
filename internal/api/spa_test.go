package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func spaFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>app</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
}

func serve(t *testing.T, e *env, method, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

func TestEmbeddedSPA(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.SPA = spaFS() })

	// The app, and client-side routes (even ones the legacy API also names).
	for _, path := range []string{"/", "/tasks", "/tasks/0193a1b2", "/admin/learning", "/signin"} {
		w := serve(t, e, "GET", path, "text/html,application/xhtml+xml")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<title>app</title>") || w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q %v", path, w.Code, w.Body.String(), w.Header())
		}
	}
	if w := serve(t, e, "HEAD", "/tasks", "text/html"); w.Code != 200 {
		t.Errorf("HEAD: %d", w.Code)
	}

	// A built file, whatever the Accept header says, cached for good.
	w := serve(t, e, "GET", "/assets/app.js", "*/*")
	if w.Code != 200 || w.Body.String() != "console.log(1)" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %v", w.Code, w.Header())
	}
	// A file that was not built is not the app.
	if w := serve(t, e, "GET", "/assets/missing.js", "*/*"); w.Code != 404 || !strings.Contains(w.Body.String(), "not_found") {
		t.Errorf("missing asset: %d %s", w.Code, w.Body.String())
	}

	// Nothing under /api/ is the app: an unknown path is the API's JSON 404,
	// a known one the API's answer, whatever the browser asks for.
	if w := serve(t, e, "GET", "/api/v1/nope", "text/html"); w.Code != 404 || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Errorf("unknown api path: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if w := serve(t, e, "GET", "/api/v1/tasks", "text/html"); w.Code != 401 {
		t.Errorf("api path: %d", w.Code)
	}
	if w := serve(t, e, "GET", "/healthz", "text/html"); w.Code != 200 || strings.Contains(w.Body.String(), "<title>") {
		t.Errorf("healthz: %d %s", w.Code, w.Body.String())
	}
	// Writes are never the app.
	if w := serve(t, e, "POST", "/tasks", "text/html"); w.Code == 200 || strings.Contains(w.Body.String(), "<title>app") {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestWithoutSPAThePathsAreTheAPIs(t *testing.T) {
	e := newEnv(t)
	if w := serve(t, e, "GET", "/", "text/html"); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not_found") {
		t.Errorf("/: %d %s", w.Code, w.Body.String())
	}
}
