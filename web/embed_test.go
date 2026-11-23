//go:build webembed

package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rtrtarun/taskify/internal/api"
	"github.com/rtrtarun/taskify/web"
)

// The tagged build holds the real built app, and serving it does not shadow
// the API. Run `npm run build` in web/ before `go test -tags webembed ./web`.
func TestTaggedBuildServesTheAppAndNotOverTheAPI(t *testing.T) {
	fsys, ok := web.Dist()
	if !ok {
		t.Fatal("the tagged build has no frontend: run `npm run build` in web/ first")
	}
	h, err := api.NewRouter(api.Deps{WebhookSecret: "s", SPA: fsys, Auth: api.AuthConfig{Algorithm: "HS256", Secret: "0123456789abcdef0123456789abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("/tasks/abc"); w.Code != 200 || !strings.Contains(w.Body.String(), `<div id="root">`) {
		t.Errorf("client route: %d %.200s", w.Code, w.Body.String())
	}
	if w := get("/api/v1/nope"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "<div") {
		t.Errorf("api path: %d %.200s", w.Code, w.Body.String())
	}
}
