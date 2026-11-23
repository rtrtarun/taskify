package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/agenttest"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

const (
	secret        = "test-jwt-secret"
	webhookSecret = "test-webhook-secret"
	tenant        = "tenant_acme"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	t       *testing.T
	handler http.Handler
	b       repotest.Backend
	mock    *mockagentpulse.Server
	apURL   string
	now     time.Time
	tasks   *service.TaskService
	deps    Deps
}

type option func(*Deps)

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	b := repotest.Memory()
	e := &env{t: t, b: b, now: time.Now().UTC().Truncate(time.Second)}
	e.mock = mockagentpulse.New()
	ts := httptest.NewServer(e.mock)
	t.Cleanup(ts.Close)
	e.apURL = ts.URL
	client := agentpulse.New(ts.URL, "k", agentpulse.WithBreaker(nil))

	clock := func() time.Time { return e.now }
	tasks := service.NewTaskService(service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments, Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs},
		service.Config{PublicBaseURL: "https://taskify.test"}, service.WithClock(clock))
	feedback := service.NewFeedbackService(b.Tasks, client, nil)
	e.tasks = tasks
	registry := tools.NewRegistry(b.Tasks)
	if err := registry.Register(agenttest.DescribeTool{Tasks: tasks, Tier2: true}); err != nil {
		t.Fatal(err)
	}
	agentJobs := service.NewAgentJobService(b.Tx, b.AgentJobs, b.Tasks, registry, nil, clock)
	d := Deps{
		AgentJobs:     agentJobs,
		Outbox:        service.NewOutboxService(b.Outbox, clock, nil),
		Contacts:      service.NewContactService(b.Tx, b.Contacts, b.Outbox, 10, clock, nil),
		Notifications: service.NewNotificationService(b.Tx, client, b.Tasks, b.Acks, b.Outbox, 10, clock, nil, nil),
		Tasks:         tasks,
		Workspaces:    service.NewWorkspaceService(b.Tx, b.Workspaces, b.Rules, client, clock),
		Feedback:      feedback,
		Webhooks:      service.NewWebhookService(b.Tx, b.Deliveries, tasks, feedback, quiet, clock).WithAgentJobs(agentJobs),
		Learning:      client,
		Auth:          AuthConfig{Algorithm: "HS256", Secret: secret},
		WebhookSecret: webhookSecret,
		RateLimit:     RateLimitConfig{RPS: 1000, Burst: 1000},
		Version:       "1.2.3",
		Logger:        quiet,
		Now:           clock,
	}
	for _, o := range opts {
		o(&d)
	}
	h, err := NewRouter(d)
	if err != nil {
		t.Fatal(err)
	}
	e.handler, e.deps = h, d
	if _, err := tasks.CreateWorkspace(context.Background(), tenant, "ws_core", "Core"); err != nil {
		t.Fatal(err)
	}
	return e
}

func token(t *testing.T, tenantID, user, role string, ttl time.Duration) string {
	t.Helper()
	c := jwt.MapClaims{"sub": user, "tenant_id": tenantID, "exp": time.Now().Add(ttl).Unix()}
	if role != "" {
		c["role"] = role
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type req struct {
	method, path string
	body         any
	headers      map[string]string
	as           string // "", "member", "admin", "other-tenant", or "none"
	remote       string
}

type resp struct {
	status int
	header http.Header
	raw    []byte
	json   map[string]any
}

func (r resp) errCode() string {
	if e, ok := r.json["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (e *env) do(r req) resp {
	e.t.Helper()
	var body io.Reader
	switch v := r.body.(type) {
	case nil:
	case string:
		body = strings.NewReader(v)
	case []byte:
		body = bytes.NewReader(v)
	default:
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	switch r.as {
	case "", "member":
		hr.Header.Set("Authorization", "Bearer "+token(e.t, tenant, "usr_bob", "member", time.Hour))
	case "admin":
		hr.Header.Set("Authorization", "Bearer "+token(e.t, tenant, "usr_admin", "admin", time.Hour))
	case "other-tenant":
		hr.Header.Set("Authorization", "Bearer "+token(e.t, "tenant_other", "usr_eve", "admin", time.Hour))
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, hr)
	out := resp{status: rec.Code, header: rec.Header(), raw: rec.Body.Bytes()}
	// M12-05: every response a handler test sees is one the contract
	// describes.
	if err := contractValidator(e.t).Check(hr, out.status, out.header, out.raw); err != nil {
		e.t.Errorf("contract: %v", err)
	}
	_ = json.Unmarshal(out.raw, &out.json)
	return out
}

func (e *env) createTask(fields map[string]any) map[string]any {
	e.t.Helper()
	body := map[string]any{"workspace_id": "ws_core", "title": "Fix checkout crash", "assignee_id": "usr_carol"}
	for k, v := range fields {
		body[k] = v
	}
	r := e.do(req{method: "POST", path: "/api/v1/tasks", body: body})
	if r.status != http.StatusCreated {
		e.t.Fatalf("create: %d %s", r.status, r.raw)
	}
	return r.json
}

func (e *env) outboxEvents(id string) []string {
	e.t.Helper()
	entries, err := e.b.Outbox.ListForAggregate(context.Background(), uuid.MustParse(id))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		out = append(out, en.EventType)
	}
	return out
}

// --- authentication --------------------------------------------------------

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	list := func(r req) resp { r.method, r.path = "GET", "/api/v1/tasks"; return e.do(r) }

	if r := list(req{as: "none"}); r.status != 401 || r.errCode() != CodeUnauthorized || r.header.Get("WWW-Authenticate") == "" {
		t.Errorf("no credentials: %d %s", r.status, r.raw)
	}
	if r := list(req{}); r.status != 200 {
		t.Errorf("valid token: %d %s", r.status, r.raw)
	}

	expired := token(t, tenant, "usr_bob", "", -time.Hour)
	if r := list(req{as: "none", headers: map[string]string{"Authorization": "Bearer " + expired}}); r.status != 401 || !strings.Contains(string(r.raw), "expired") {
		t.Errorf("expired token: %d %s", r.status, r.raw)
	}
	wrongKey, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u", "tenant_id": tenant, "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte("other"))
	if r := list(req{as: "none", headers: map[string]string{"Authorization": "Bearer " + wrongKey}}); r.status != 401 {
		t.Errorf("wrongly signed token: %d", r.status)
	}
	unsigned, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "u", "tenant_id": tenant, "exp": time.Now().Add(time.Hour).Unix()}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if r := list(req{as: "none", headers: map[string]string{"Authorization": "Bearer " + unsigned}}); r.status != 401 {
		t.Errorf("alg none token: %d", r.status)
	}
	noExp, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u", "tenant_id": tenant}).SignedString([]byte(secret))
	if r := list(req{as: "none", headers: map[string]string{"Authorization": "Bearer " + noExp}}); r.status != 401 {
		t.Errorf("token without expiry: %d", r.status)
	}
	noTenant := token(t, "", "usr_bob", "", time.Hour)
	if r := list(req{as: "none", headers: map[string]string{"Authorization": "Bearer " + noTenant}}); r.status != 401 {
		t.Errorf("token without tenant: %d", r.status)
	}
	if r := list(req{as: "none", headers: map[string]string{"Authorization": "Basic abc"}}); r.status != 401 {
		t.Errorf("basic auth: %d", r.status)
	}
	// A bad token never falls back to identity headers, even in development.
	dev := newEnv(t, func(d *Deps) { d.Auth.Development = true })
	r := dev.do(req{method: "GET", path: "/api/v1/tasks", as: "none", headers: map[string]string{
		"Authorization": "Bearer junk", HeaderTenantID: tenant, HeaderUserID: "usr_bob"}})
	if r.status != 401 {
		t.Errorf("bad token plus headers: %d", r.status)
	}
}

func TestIdentityHeaders(t *testing.T) {
	headers := map[string]string{HeaderTenantID: tenant, HeaderUserID: "usr_bob"}
	gw := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	prod := newEnv(t, func(d *Deps) { d.Auth.TrustedGatewayCIDRs = gw })
	get := func(e *env, remote string, extra map[string]string) resp {
		h := map[string]string{}
		for k, v := range headers {
			h[k] = v
		}
		for k, v := range extra {
			h[k] = v
		}
		return e.do(req{method: "GET", path: "/api/v1/tasks", as: "none", headers: h, remote: remote})
	}

	if r := get(prod, "203.0.113.9:5555", nil); r.status != 401 {
		t.Errorf("production, untrusted address: %d", r.status)
	}
	if r := get(prod, "203.0.113.9:5555", map[string]string{"X-Forwarded-For": "10.1.2.3", "X-Real-IP": "10.1.2.3"}); r.status != 401 {
		t.Errorf("spoofed X-Forwarded-For was trusted: %d", r.status)
	}
	if r := get(prod, "10.1.2.3:5555", nil); r.status != 200 {
		t.Errorf("trusted gateway: %d %s", r.status, r.raw)
	}
	if r := get(prod, "[::ffff:10.1.2.3]:5555", nil); r.status != 200 {
		t.Errorf("trusted gateway over IPv4-mapped IPv6: %d", r.status)
	}
	dev := newEnv(t, func(d *Deps) { d.Auth.Development = true })
	if r := get(dev, "203.0.113.9:5555", nil); r.status != 200 {
		t.Errorf("development: %d", r.status)
	}
	// The role header grants admin only through a trusted path.
	if r := dev.do(req{method: "GET", path: "/api/v1/admin/learning/suggestions", as: "none",
		headers: map[string]string{HeaderTenantID: tenant, HeaderUserID: "u", HeaderUserRole: "admin"}}); r.status != 200 {
		t.Errorf("development admin header: %d %s", r.status, r.raw)
	}
}

func TestRS256(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	e := newEnv(t, func(d *Deps) { d.Auth = AuthConfig{Algorithm: "RS256", PublicKey: pemKey} })

	signed, _ := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"sub": "u", "tenant_id": tenant, "exp": time.Now().Add(time.Hour).Unix()}).SignedString(key)
	if r := e.do(req{method: "GET", path: "/api/v1/tasks", as: "none", headers: map[string]string{"Authorization": "Bearer " + signed}}); r.status != 200 {
		t.Errorf("RS256 token: %d %s", r.status, r.raw)
	}
	// An HS256 token signed with the public key text must not pass (key confusion).
	confused, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u", "tenant_id": tenant, "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte(pemKey))
	if r := e.do(req{method: "GET", path: "/api/v1/tasks", as: "none", headers: map[string]string{"Authorization": "Bearer " + confused}}); r.status != 401 {
		t.Errorf("HS256 token against an RS256 server: %d", r.status)
	}
	if _, err := NewRouter(Deps{WebhookSecret: "x", Auth: AuthConfig{Algorithm: "RS256", PublicKey: "not a key"}}); err == nil {
		t.Error("a malformed public key was accepted")
	}
	if _, err := NewRouter(Deps{WebhookSecret: "x", Auth: AuthConfig{Algorithm: "ES256"}}); err == nil {
		t.Error("an unsupported algorithm was accepted")
	}
	if _, err := NewRouter(Deps{}); err == nil {
		t.Error("a router without a webhook secret was built")
	}
}

// --- tasks -----------------------------------------------------------------

func TestCreateTask(t *testing.T) {
	e := newEnv(t)
	due := e.now.Add(48 * time.Hour).Format(time.RFC3339)
	r := e.do(req{method: "POST", path: "/api/v1/tasks", body: map[string]any{
		"workspace_id": "ws_core", "title": "Fix checkout crash", "priority": "p2_high",
		"assignee_id": "usr_carol", "labels": []string{"bug"}, "watcher_ids": []string{"usr_dave"},
		"metadata": map[string]any{"source": "api"}, "due_at": due,
	}})
	if r.status != 201 {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	id := r.json["id"].(string)
	if r.header.Get("ETag") != `"1"` || r.header.Get("Location") != "/api/v1/tasks/"+id {
		t.Errorf("headers: %v", r.header)
	}
	j := r.json
	if j["tenant_id"] != tenant || j["creator_id"] != "usr_bob" || j["status"] != "todo" || j["priority"] != "p2_high" ||
		j["due_at"] != due || j["version"] != 1.0 || j["completed_at"] != nil || j["blocked_reason"] != nil {
		t.Errorf("body: %s", r.raw)
	}
	if !strings.HasSuffix(j["created_at"].(string), "Z") {
		t.Errorf("created_at is not UTC: %v", j["created_at"])
	}
	if got := e.outboxEvents(id); len(got) != 1 || got[0] != domain.EventTaskCreated {
		t.Errorf("outbox = %v", got)
	}

	for name, tc := range map[string]struct {
		body   any
		status int
		code   string
	}{
		"malformed JSON":    {`{"title":`, 400, CodeInvalidRequest},
		"unknown field":     {map[string]any{"workspace_id": "ws_core", "title": "x", "titel": "y"}, 400, CodeInvalidRequest},
		"two objects":       {`{"workspace_id":"ws_core","title":"x"}{}`, 400, CodeInvalidRequest},
		"bad timestamp":     {map[string]any{"workspace_id": "ws_core", "title": "x", "due_at": "tomorrow"}, 400, CodeInvalidRequest},
		"missing title":     {map[string]any{"workspace_id": "ws_core"}, 422, CodeValidation},
		"bad priority":      {map[string]any{"workspace_id": "ws_core", "title": "x", "priority": "urgent"}, 422, CodeValidation},
		"unknown workspace": {map[string]any{"workspace_id": "ws_nope", "title": "x"}, 422, CodeValidation},
		"oversized body":    {`{"title":"` + strings.Repeat("x", maxBody) + `"}`, 413, CodeInvalidRequest},
	} {
		r := e.do(req{method: "POST", path: "/api/v1/tasks", body: tc.body})
		if r.status != tc.status || r.errCode() != tc.code {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}

	// The tenant comes from the token, not the request.
	r = e.do(req{method: "POST", path: "/api/v1/tasks", body: map[string]any{"workspace_id": "ws_core", "title": "x", "tenant_id": "tenant_other"}})
	if r.status != 400 {
		t.Errorf("tenant_id in body: %d", r.status)
	}
}

func TestGetTask(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := task["id"].(string)
	e.do(req{method: "POST", path: "/api/v1/tasks/" + id + "/comments", body: map[string]any{"body": "first"}})

	r := e.do(req{method: "GET", path: "/api/v1/tasks/" + id})
	if tag := r.header.Get("ETag"); r.status != 200 || !strings.HasPrefix(tag, `"1.`) {
		t.Fatalf("get: %d %s %s", r.status, tag, r.raw)
	}
	comments := r.json["comments"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["body"] != "first" || r.json["title"] != "Fix checkout crash" {
		t.Errorf("body: %s", r.raw)
	}
	for name, rq := range map[string]req{
		"unknown ID":    {method: "GET", path: "/api/v1/tasks/" + uuid.NewString()},
		"malformed ID":  {method: "GET", path: "/api/v1/tasks/not-a-uuid"},
		"other tenant":  {method: "GET", path: "/api/v1/tasks/" + id, as: "other-tenant"},
		"update other":  {method: "PATCH", path: "/api/v1/tasks/" + id, as: "other-tenant", body: map[string]any{"version": 1, "title": "x"}},
		"comment other": {method: "POST", path: "/api/v1/tasks/" + id + "/comments", as: "other-tenant", body: map[string]any{"body": "x"}},
	} {
		if r := e.do(rq); r.status != 404 || r.errCode() != CodeNotFound {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
}

func TestListTasks(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.now = e.now.Add(time.Second)
		fields := map[string]any{"title": "task " + strconv.Itoa(i)}
		if i%2 == 0 {
			fields["priority"] = "p1_critical"
		}
		e.createTask(fields)
	}
	r := e.do(req{method: "GET", path: "/api/v1/tasks?limit=2"})
	if r.status != 200 || len(r.json["tasks"].([]any)) != 2 || r.json["next_cursor"] == nil {
		t.Fatalf("first page: %d %s", r.status, r.raw)
	}
	seen := 2
	cursor := r.json["next_cursor"].(string)
	for cursor != "" {
		r = e.do(req{method: "GET", path: "/api/v1/tasks?limit=2&cursor=" + cursor})
		if r.status != 200 {
			t.Fatalf("page: %d %s", r.status, r.raw)
		}
		seen += len(r.json["tasks"].([]any))
		cursor, _ = r.json["next_cursor"].(string)
	}
	if seen != 5 {
		t.Errorf("paged through %d tasks, want 5", seen)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?priority=p1_critical&assignee_id=usr_carol&status=todo"}); len(r.json["tasks"].([]any)) != 3 {
		t.Errorf("filtered: %s", r.raw)
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=ten", "cursor=%25%25", "cursor=" + "bm90LWEtY3Vyc29y"} {
		if r := e.do(req{method: "GET", path: "/api/v1/tasks?" + q}); r.status != 400 {
			t.Errorf("%s: %d", q, r.status)
		}
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?status=done"}); r.status != 422 {
		t.Errorf("bad status filter: %d", r.status)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks", as: "other-tenant"}); len(r.json["tasks"].([]any)) != 0 {
		t.Errorf("another tenant sees tasks: %s", r.raw)
	}
}

func TestUpdateTask(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(map[string]any{"due_at": e.now.Add(time.Hour).Format(time.RFC3339)})
	path := "/api/v1/tasks/" + task["id"].(string)

	r := e.do(req{method: "PATCH", path: path, headers: map[string]string{"If-Match": `"1"`},
		body: map[string]any{"title": "Renamed", "priority": "p1_critical", "assignee_id": nil, "due_at": nil}})
	if r.status != 200 || r.json["title"] != "Renamed" || r.json["version"] != 2.0 || r.header.Get("ETag") != `"2"` {
		t.Fatalf("patch: %d %s", r.status, r.raw)
	}
	if r.json["assignee_id"] != nil || r.json["due_at"] != nil {
		t.Errorf("null did not clear: %s", r.raw)
	}

	// The version may come in the body instead, and PUT behaves the same.
	if r := e.do(req{method: "PUT", path: path, body: map[string]any{"version": 2, "description": "Detail"}}); r.status != 200 || r.json["version"] != 3.0 {
		t.Errorf("PUT with body version: %d %s", r.status, r.raw)
	}

	stale := e.do(req{method: "PATCH", path: path, headers: map[string]string{"If-Match": "1"}, body: map[string]any{"title": "Stale"}})
	if stale.status != 409 || stale.errCode() != CodeVersionConflict {
		t.Fatalf("stale: %d %s", stale.status, stale.raw)
	}
	if d := stale.json["error"].(map[string]any)["details"].(map[string]any); d["current_version"] != 3.0 {
		t.Errorf("details = %v", d)
	}

	for name, tc := range map[string]struct {
		headers map[string]string
		body    any
		status  int
		code    string
	}{
		"no version":        {nil, map[string]any{"title": "x"}, 428, CodeVersionRequired},
		"versions disagree": {map[string]string{"If-Match": `"3"`}, map[string]any{"version": 2, "title": "x"}, 400, CodeInvalidRequest},
		"bad If-Match":      {map[string]string{"If-Match": "*"}, map[string]any{"title": "x"}, 400, CodeInvalidRequest},
		"zero version":      {nil, map[string]any{"version": 0, "title": "x"}, 400, CodeInvalidRequest},
		"null title":        {map[string]string{"If-Match": "3"}, map[string]any{"title": nil}, 422, CodeValidation},
		"null status":       {map[string]string{"If-Match": "3"}, map[string]any{"status": nil}, 422, CodeValidation},
		"bad transition":    {map[string]string{"If-Match": "3"}, map[string]any{"status": "archived"}, 0, ""},
		"unknown status":    {map[string]string{"If-Match": "3"}, map[string]any{"status": "done"}, 422, CodeValidation},
		"wrong type":        {map[string]string{"If-Match": "3"}, map[string]any{"labels": "bug"}, 400, CodeInvalidRequest},
	} {
		if tc.status == 0 {
			continue // covered below
		}
		r := e.do(req{method: "PATCH", path: path, headers: tc.headers, body: tc.body})
		if r.status != tc.status || r.errCode() != tc.code {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}

	// Run last: it succeeds and moves the task past version 3, which every
	// case above relies on.
	if r := e.do(req{method: "PATCH", path: path, headers: map[string]string{"If-Match": `W/"3"`}, body: map[string]any{"labels": []string{"bug"}}}); r.status != 200 {
		t.Errorf("weak If-Match form: %d %s", r.status, r.raw)
	}

	// completed cannot go to blocked.
	cur := e.do(req{method: "GET", path: path}).json["version"].(float64)
	e.do(req{method: "POST", path: path + "/complete"})
	r = e.do(req{method: "PATCH", path: path, body: map[string]any{"version": cur + 1, "status": "blocked"}})
	if r.status != 422 || r.errCode() != CodeInvalidTransition {
		t.Errorf("invalid transition: %d %s", r.status, r.raw)
	}
}

func TestCompleteArchiveRestoreDelete(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := task["id"].(string)
	path := "/api/v1/tasks/" + id

	// "Mark done" on a todo task by someone other than its creator, with a
	// notification ID: one task.completed to the creator and one acted signal.
	e.mock.SeedNotification("ntf_7", map[string]any{"tenantId": tenant, "userId": "usr_admin"})
	r := e.do(req{method: "POST", path: path + "/complete", as: "admin", body: map[string]any{"notification_id": "ntf_7"}})
	if r.status != 200 || r.json["status"] != "completed" || r.json["completed_at"] == nil {
		t.Fatalf("complete: %d %s", r.status, r.raw)
	}
	completed := 0
	for _, ev := range e.outboxEvents(id) {
		if ev == domain.EventTaskCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Errorf("task.completed rows = %d, want 1", completed)
	}
	if fb := e.mock.Feedback(); len(fb) != 1 || fb[0]["signal"] != "acted" || fb[0]["notificationId"] != "ntf_7" {
		t.Errorf("feedback = %v", fb)
	}
	if r := e.do(req{method: "POST", path: path + "/complete", headers: map[string]string{"If-Match": "1"}}); r.status != 409 {
		t.Errorf("complete with a stale If-Match: %d", r.status)
	}

	// Archive: gone from the list, still fetchable, restorable.
	if r := e.do(req{method: "DELETE", path: path}); r.status != 200 || r.json["status"] != "archived" {
		t.Fatalf("archive: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks"}); len(r.json["tasks"].([]any)) != 0 {
		t.Error("an archived task is listed")
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks?status=archived"}); len(r.json["tasks"].([]any)) != 1 {
		t.Error("an archived task is not listed under status=archived")
	}
	if r := e.do(req{method: "GET", path: path}); r.status != 200 {
		t.Errorf("get archived: %d", r.status)
	}
	if r := e.do(req{method: "POST", path: path + "/restore"}); r.status != 200 || r.json["status"] != "todo" {
		t.Fatalf("restore: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "POST", path: path + "/restore"}); r.status != 422 || r.errCode() != CodeInvalidTransition {
		t.Errorf("restore of a todo task: %d %s", r.status, r.raw)
	}

	// Permanent delete is for admins only.
	if r := e.do(req{method: "DELETE", path: path + "?permanent=true"}); r.status != 403 || r.errCode() != CodeForbidden {
		t.Errorf("member permanent delete: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "DELETE", path: path + "?permanent=yes", as: "admin"}); r.status != 400 {
		t.Errorf("bad permanent value: %d", r.status)
	}
	if r := e.do(req{method: "DELETE", path: path + "?permanent=true", as: "admin"}); r.status != 204 || len(r.raw) != 0 {
		t.Fatalf("admin permanent delete: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: path}); r.status != 404 {
		t.Errorf("get after delete: %d", r.status)
	}
	if got := e.outboxEvents(id); len(got) != 0 {
		t.Errorf("outbox rows survived: %v", got)
	}
}

func TestCommentsAndFeedback(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	path := "/api/v1/tasks/" + task["id"].(string)

	r := e.do(req{method: "POST", path: path + "/comments", body: map[string]any{"body": "@usr_dave please check"}})
	if r.status != 201 || r.json["author_id"] != "usr_bob" {
		t.Fatalf("comment: %d %s", r.status, r.raw)
	}
	if m := r.json["mentions"].([]any); len(m) != 1 || m[0] != "usr_dave" {
		t.Errorf("mentions = %v", m)
	}
	if r := e.do(req{method: "POST", path: path + "/comments", body: map[string]any{"body": " "}}); r.status != 422 {
		t.Errorf("empty comment: %d", r.status)
	}

	// As an actionUrl: the signal is in the query string, with no body.
	r = e.do(req{method: "POST", path: path + "/feedback?signal=snoozed&duration=2h"})
	if r.status != 202 {
		t.Fatalf("feedback from query: %d %s", r.status, r.raw)
	}
	e.mock.SeedNotification("ntf_3", map[string]any{"tenantId": tenant, "userId": "usr_bob"})
	r = e.do(req{method: "POST", path: path + "/feedback", body: map[string]any{"signal": "dismissed", "notification_id": "ntf_3"}})
	if r.status != 202 {
		t.Fatalf("feedback from body: %d %s", r.status, r.raw)
	}
	fb := e.mock.Feedback()
	if len(fb) != 2 || fb[0]["snoozeDuration"] != "2h" || fb[1]["signal"] != "dismissed" || fb[1]["notificationId"] != "ntf_3" {
		t.Errorf("feedback = %v", fb)
	}
	if r := e.do(req{method: "POST", path: path + "/feedback", body: map[string]any{"signal": "loved"}}); r.status != 422 {
		t.Errorf("bad signal: %d", r.status)
	}
	e.mock.FailNext("POST", "/v1/feedback", http.StatusServiceUnavailable, 1)
	if r := e.do(req{method: "POST", path: path + "/feedback?signal=acted"}); r.status != 502 || r.errCode() != CodeUpstreamError {
		t.Errorf("AgentPulse down: %d %s", r.status, r.raw)
	}
}

func TestLegacyRoutes(t *testing.T) {
	e := newEnv(t)
	e.createTask(nil)
	v1 := e.do(req{method: "GET", path: "/api/v1/tasks"})
	for _, p := range []string{"/tasks", "/tasks/"} {
		legacy := e.do(req{method: "GET", path: p})
		if legacy.status != 200 || !bytes.Equal(legacy.raw, v1.raw) {
			t.Errorf("GET %s: %d, body differs from /api/v1/tasks", p, legacy.status)
		}
	}
	if r := e.do(req{method: "GET", path: "/tasks", as: "none"}); r.status != 401 {
		t.Errorf("legacy route without auth: %d", r.status)
	}
	r := e.do(req{method: "POST", path: "/tasks", body: map[string]any{"workspace_id": "ws_core", "title": "legacy"}})
	if r.status != 201 {
		t.Errorf("legacy create: %d %s", r.status, r.raw)
	}
}

// --- webhook ---------------------------------------------------------------

func (e *env) webhook(body any, ts int64, sig string) resp {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	if s, ok := body.(string); ok {
		raw = []byte(s)
	}
	if sig == "" {
		sig = SignWebhook(webhookSecret, ts, raw)
	}
	return e.do(req{method: "POST", path: "/api/v1/webhooks/agentpulse", as: "none", body: raw, headers: map[string]string{
		HeaderWebhookTimestamp: strconv.FormatInt(ts, 10), HeaderWebhookSignature: sig,
	}})
}

func TestWebhook(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := task["id"].(string)
	now := e.now.Unix()
	body := map[string]any{"action": "complete", "tenantId": tenant, "userId": "usr_carol", "taskId": id, "notificationId": "ntf_1", "extra": "ignored"}
	e.mock.SeedNotification("ntf_1", map[string]any{"tenantId": tenant, "userId": "usr_carol"})

	r := e.webhook(body, now, "")
	if r.status != 200 || r.json["status"] != "applied" {
		t.Fatalf("valid callback: %d %s", r.status, r.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/tasks/" + id}); got.json["status"] != "completed" {
		t.Errorf("task status = %v", got.json["status"])
	}
	if fb := e.mock.Feedback(); len(fb) != 1 || fb[0]["signal"] != "acted" || fb[0]["userId"] != "usr_carol" {
		t.Errorf("feedback = %v", fb)
	}
	if r := e.webhook(body, now, ""); r.status != 200 || r.json["status"] != "duplicate" {
		t.Errorf("replay: %d %s", r.status, r.raw)
	}

	raw, _ := json.Marshal(body)
	good := SignWebhook(webhookSecret, now, raw)
	for name, tc := range map[string]struct {
		ts  int64
		sig string
	}{
		"bad signature":    {now, strings.Repeat("ab", 32)},
		"wrong secret":     {now, SignWebhook("other", now, raw)},
		"malformed sig":    {now, "zz"},
		"stale timestamp":  {now - 301, SignWebhook(webhookSecret, now-301, raw)},
		"future timestamp": {now + 301, SignWebhook(webhookSecret, now+301, raw)},
		"signature reused": {now + 1, good}, // valid signature, different timestamp
	} {
		if r := e.webhook(body, tc.ts, tc.sig); r.status != 401 || r.errCode() != CodeUnauthorized {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	missing := e.do(req{method: "POST", path: "/api/v1/webhooks/agentpulse", as: "none", body: raw})
	if missing.status != 401 {
		t.Errorf("no signature headers: %d", missing.status)
	}

	// Accepted forms and content errors behind a valid signature.
	snooze := map[string]any{"action": "snooze", "tenantId": tenant, "userId": "usr_carol", "taskId": id, "duration": "2h"}
	sraw, _ := json.Marshal(snooze)
	if r := e.webhook(snooze, now, "sha256="+SignWebhook(webhookSecret, now, sraw)); r.status != 200 {
		t.Errorf("sha256= prefix: %d %s", r.status, r.raw)
	}
	for name, tc := range map[string]struct {
		body   any
		status int
	}{
		"not JSON":       {"{nope", 400},
		"unknown action": {map[string]any{"action": "explode", "tenantId": tenant, "userId": "u", "taskId": id}, 422},
		"other tenant":   {map[string]any{"action": "complete", "tenantId": "tenant_other", "userId": "u", "taskId": id}, 404},
		"bad task ID":    {map[string]any{"action": "complete", "tenantId": tenant, "userId": "u", "taskId": "x"}, 404},
	} {
		if r := e.webhook(tc.body, now, ""); r.status != tc.status {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
}

// TestWebhookUnblock is the M9 exit criterion "clicking the unblock action
// moves the task to in_progress through the webhook path" (M9-04).
func TestWebhookUnblock(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := task["id"].(string)
	if r := e.do(req{method: "PATCH", path: "/api/v1/tasks/" + id, body: map[string]any{"version": 1, "status": "blocked"}}); r.status != 200 {
		t.Fatalf("block: %d %s", r.status, r.raw)
	}
	now := e.now.Unix()
	body := map[string]any{"action": "unblock", "tenantId": tenant, "userId": "usr_dave", "taskId": id, "notificationId": "ntf_esc"}
	e.mock.SeedNotification("ntf_esc", map[string]any{"tenantId": tenant, "userId": "usr_dave"})
	if r := e.webhook(body, now, ""); r.status != 200 || r.json["status"] != "applied" {
		t.Fatalf("unblock callback: %d %s", r.status, r.raw)
	}
	got := e.do(req{method: "GET", path: "/api/v1/tasks/" + id})
	if got.json["status"] != "in_progress" || got.json["blocked_reason"] != nil {
		t.Errorf("task after unblock = %v, %v", got.json["status"], got.json["blocked_reason"])
	}
	fb := e.mock.Feedback()
	if len(fb) != 1 || fb[0]["signal"] != "acted" || fb[0]["userId"] != "usr_dave" || fb[0]["notificationId"] != "ntf_esc" {
		t.Errorf("feedback = %v", fb)
	}
	if r := e.webhook(body, now, ""); r.status != 200 || r.json["status"] != "duplicate" {
		t.Errorf("replay: %d %s", r.status, r.raw)
	}
	// A second click, signed anew, finds the task in progress and changes nothing.
	if r := e.webhook(body, now+1, ""); r.status != 200 {
		t.Errorf("second click: %d %s", r.status, r.raw)
	}
	if again := e.do(req{method: "GET", path: "/api/v1/tasks/" + id}); again.json["version"] != got.json["version"] {
		t.Errorf("second click moved the version to %v", again.json["version"])
	}
	// A completed task cannot be unblocked.
	done := e.createTask(nil)
	doneID := done["id"].(string)
	if r := e.webhook(map[string]any{"action": "complete", "tenantId": tenant, "userId": "usr_carol", "taskId": doneID}, now, ""); r.status != 200 {
		t.Fatalf("complete: %d %s", r.status, r.raw)
	}
	if r := e.webhook(map[string]any{"action": "unblock", "tenantId": tenant, "userId": "usr_carol", "taskId": doneID}, now, ""); r.status != 422 {
		t.Errorf("unblock a completed task: %d %s", r.status, r.raw)
	}
}

// --- workspaces and learning ----------------------------------------------

func TestAdminRoutesNeedAdmin(t *testing.T) {
	e := newEnv(t)
	for _, rq := range []req{
		{method: "POST", path: "/api/v1/workspaces", body: map[string]any{"id": "w", "name": "n"}},
		{method: "GET", path: "/api/v1/workspaces/ws_core/settings"},
		{method: "PATCH", path: "/api/v1/workspaces/ws_core/settings", body: map[string]any{"name": "n"}},
		{method: "POST", path: "/api/v1/workspaces/ws_core/bootstrap-rules"},
		{method: "GET", path: "/api/v1/admin/learning/suggestions"},
		{method: "POST", path: "/api/v1/admin/learning/suggestions/x/decision", body: map[string]any{"decision": "approve"}},
		{method: "GET", path: "/api/v1/admin/outbox"},
		{method: "POST", path: "/api/v1/admin/outbox/" + uuid.NewString() + "/replay"},
	} {
		if r := e.do(rq); r.status != 403 || r.errCode() != CodeForbidden {
			t.Errorf("%s %s as member: %d", rq.method, rq.path, r.status)
		}
	}
}

func TestWorkspaces(t *testing.T) {
	e := newEnv(t)
	r := e.do(req{method: "POST", path: "/api/v1/workspaces", as: "admin", body: map[string]any{"id": "ws_new", "name": "New"}})
	if r.status != 201 || r.json["rules_bootstrapped"] != true || len(r.json["rules"].([]any)) != 3 || r.json["id"] != "ws_new" {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	if n := len(e.mock.Rules()); n != 3 {
		t.Errorf("AgentPulse received %d rules", n)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/workspaces/ws_new/bootstrap-rules", as: "admin"}); r.status != 200 || len(r.json["rules"].([]any)) != 3 {
		t.Errorf("bootstrap again: %d %s", r.status, r.raw)
	}
	if n := len(e.mock.Rules()); n != 3 {
		t.Errorf("a second bootstrap sent more rules: %d", n)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/workspaces", as: "admin", body: map[string]any{"id": "ws_new", "name": "Dup"}}); r.status != 409 {
		t.Errorf("duplicate: %d", r.status)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/workspaces", as: "admin", body: map[string]any{"id": "bad id", "name": "x"}}); r.status != 422 {
		t.Errorf("bad id: %d", r.status)
	}

	// AgentPulse down: the workspace is still created, and says so.
	e.mock.FailNext("POST", "/v1/rules", http.StatusServiceUnavailable, 1)
	r = e.do(req{method: "POST", path: "/api/v1/workspaces", as: "admin", body: map[string]any{"id": "ws_offline", "name": "Offline"}})
	if r.status != 201 || r.json["rules_bootstrapped"] != false || r.json["rules_error"] == "" {
		t.Errorf("create with AgentPulse down: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/workspaces/ws_offline/bootstrap-rules", as: "admin"}); r.status != 200 || len(r.json["rules"].([]any)) != 3 {
		t.Errorf("bootstrap after recovery: %d %s", r.status, r.raw)
	}

	if r := e.do(req{method: "GET", path: "/api/v1/workspaces/ws_new/settings", as: "admin"}); r.status != 200 || r.json["name"] != "New" {
		t.Errorf("settings: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "PATCH", path: "/api/v1/workspaces/ws_new/settings", as: "admin", body: map[string]any{"name": "Renamed"}}); r.status != 200 || r.json["name"] != "Renamed" {
		t.Errorf("rename: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "PATCH", path: "/api/v1/workspaces/ws_new/settings", as: "admin", body: map[string]any{}}); r.status != 200 || r.json["name"] != "Renamed" {
		t.Errorf("empty patch: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/workspaces/ws_new/settings", as: "other-tenant"}); r.status != 404 {
		t.Errorf("another tenant's workspace: %d", r.status)
	}

	// Agent settings (M5-05): a new workspace has agents off and approval for
	// everything on.
	r = e.do(req{method: "GET", path: "/api/v1/workspaces/ws_new/settings", as: "admin"})
	agents, _ := r.json["agent_settings"].(map[string]any)
	if r.status != 200 || agents["agents_enabled"] != false || agents["require_hitl_for_all"] != true ||
		agents["max_subtasks_per_decomposition"] != float64(5) || len(agents["disabled_agents"].([]any)) != 0 {
		t.Errorf("default agent settings: %d %s", r.status, r.raw)
	}
	r = e.do(req{method: "PATCH", path: "/api/v1/workspaces/ws_new/settings", as: "admin", body: map[string]any{
		"agent_settings": map[string]any{"agents_enabled": true, "disabled_agents": []string{"decomposer"}},
	}})
	agents, _ = r.json["agent_settings"].(map[string]any)
	if r.status != 200 || agents["agents_enabled"] != true || agents["require_hitl_for_all"] != true || r.json["name"] != "Renamed" {
		t.Errorf("enable agents: %d %s", r.status, r.raw)
	}
	for name, body := range map[string]map[string]any{
		"unknown agent":           {"agent_settings": map[string]any{"disabled_agents": []string{"oracle"}}},
		"zero cap":                {"agent_settings": map[string]any{"max_subtasks_per_decomposition": 0}},
		"bad name, good settings": {"name": " ", "agent_settings": map[string]any{"require_hitl_for_all": false}},
	} {
		if r := e.do(req{method: "PATCH", path: "/api/v1/workspaces/ws_new/settings", as: "admin", body: body}); r.status != 422 {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	// A refused patch changed nothing, including its valid half.
	r = e.do(req{method: "GET", path: "/api/v1/workspaces/ws_new/settings", as: "admin"})
	agents, _ = r.json["agent_settings"].(map[string]any)
	if agents["require_hitl_for_all"] != true || agents["max_subtasks_per_decomposition"] != float64(5) || r.json["name"] != "Renamed" {
		t.Errorf("after refused patches: %s", r.raw)
	}
	if r := e.do(req{method: "PATCH", path: "/api/v1/workspaces/ws_new/settings", body: map[string]any{
		"agent_settings": map[string]any{"agents_enabled": false},
	}}); r.status != 403 {
		t.Errorf("non-admin changed agent settings: %d", r.status)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/workspaces/ws_missing/bootstrap-rules", as: "admin"}); r.status != 404 {
		t.Errorf("bootstrap missing: %d", r.status)
	}
}

func TestLearningProxy(t *testing.T) {
	e := newEnv(t)
	id := e.mock.SeedSuggestion(map[string]any{"tenantId": tenant, "eventPattern": map[string]any{"eventType": "task.updated", "label": nil},
		"currentAdjustment": 0.0, "proposedAdjustment": -5.0, "signalCount": 7.0, "rationale": "Dismissed by most.", "futureField": "x"})
	byRule := e.mock.SeedSuggestion(map[string]any{"tenantId": tenant, "ruleId": "rule_9", "proposedAdjustment": 4.0})
	others := e.mock.SeedSuggestion(map[string]any{"tenantId": "tenant_other", "proposedAdjustment": 3.0})

	// snake_case, and only the fields the contract lists (rule 9).
	r := e.do(req{method: "GET", path: "/api/v1/admin/learning/suggestions", as: "admin"})
	list, _ := r.json["suggestions"].([]any)
	if r.status != 200 || len(list) != 2 {
		t.Fatalf("list: %d %s", r.status, r.raw)
	}
	byPattern, ruled := list[0].(map[string]any), list[1].(map[string]any)
	if byPattern["id"] != id || byPattern["status"] != "PENDING_APPROVAL" || byPattern["rule_id"] != nil ||
		byPattern["proposed_adjustment"] != -5.0 || byPattern["signal_count"] != 7.0 || byPattern["rationale"] != "Dismissed by most." {
		t.Errorf("by pattern = %v", byPattern)
	}
	if p, _ := byPattern["event_pattern"].(map[string]any); p["event_type"] != "task.updated" || p["label"] != nil || len(p) != 2 {
		t.Errorf("event_pattern = %v", byPattern["event_pattern"])
	}
	if ruled["id"] != byRule || ruled["rule_id"] != "rule_9" || ruled["event_pattern"] != nil {
		t.Errorf("by rule = %v", ruled)
	}
	for _, camel := range []string{"suggestion", "proposedAdjustment", "eventPattern", "futureField", "tenantId"} {
		if strings.Contains(string(r.raw), `"`+camel+`"`) {
			t.Errorf("the list carries %q: %s", camel, r.raw)
		}
	}
	r = e.do(req{method: "POST", path: "/api/v1/admin/learning/suggestions/" + id + "/decision", as: "admin", body: map[string]any{"decision": "approve"}})
	if r.status != 200 || r.json["status"] != "APPROVED" || r.json["id"] != id || r.json["proposed_adjustment"] != -5.0 {
		t.Fatalf("approve: %d %s", r.status, r.raw)
	}
	sent := e.mock.RequestsTo("POST", "/v1/learning/suggestions/"+id+"/decision")
	if len(sent) != 1 || !strings.Contains(string(sent[0].Body), `"tenantId":"`+tenant+`"`) {
		t.Errorf("decision requests = %v", sent)
	}
	// AP-12: the decision names the admin's tenant, so another tenant's
	// suggestion is not found and stays undecided.
	if r := e.do(req{method: "POST", path: "/api/v1/admin/learning/suggestions/" + others + "/decision", as: "admin", body: map[string]any{"decision": "approve"}}); r.status != 404 {
		t.Errorf("another tenant's suggestion: %d %s", r.status, r.raw)
	}
	for _, sg := range e.mock.Suggestions() {
		if sg.ID == others && sg.Status != "PENDING_APPROVAL" {
			t.Errorf("another tenant's suggestion was decided: %s", sg.Status)
		}
	}
	if r := e.do(req{method: "POST", path: "/api/v1/admin/learning/suggestions/" + id + "/decision", as: "admin", body: map[string]any{"decision": "maybe"}}); r.status != 422 {
		t.Errorf("bad decision: %d", r.status)
	}
	if r := e.do(req{method: "POST", path: "/api/v1/admin/learning/suggestions/missing/decision", as: "admin", body: map[string]any{"decision": "reject"}}); r.status != 404 {
		t.Errorf("unknown suggestion: %d %s", r.status, r.raw)
	}
	e.mock.FailNext("GET", "/v1/learning/suggestions", http.StatusInternalServerError, 1)
	if r := e.do(req{method: "GET", path: "/api/v1/admin/learning/suggestions", as: "admin"}); r.status != 502 {
		t.Errorf("AgentPulse error: %d", r.status)
	}
}

// --- middleware ------------------------------------------------------------

func TestRateLimit(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.RateLimit = RateLimitConfig{RPS: 0.001, Burst: 2} })
	for i := 0; i < 2; i++ {
		if r := e.do(req{method: "GET", path: "/api/v1/tasks"}); r.status != 200 {
			t.Fatalf("request %d: %d", i, r.status)
		}
	}
	r := e.do(req{method: "GET", path: "/api/v1/tasks"})
	if r.status != 429 || r.errCode() != CodeRateLimited || r.header.Get("Retry-After") == "" {
		t.Errorf("over the limit: %d %v", r.status, r.header)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks", as: "admin"}); r.status != 200 {
		t.Errorf("another user was limited: %d", r.status)
	}
}

func TestHealthRequestIDAndRouting(t *testing.T) {
	e := newEnv(t)
	r := e.do(req{method: "GET", path: "/healthz", as: "none"})
	if r.status != 200 || r.json["status"] != "ok" || r.json["version"] != "1.2.3" {
		t.Errorf("healthz: %d %s", r.status, r.raw)
	}
	if id := r.header.Get(HeaderRequestID); len(id) != 36 {
		t.Errorf("generated request ID = %q", id)
	}
	r = e.do(req{method: "GET", path: "/healthz", as: "none", headers: map[string]string{HeaderRequestID: "trace-abc.1"}})
	if r.header.Get(HeaderRequestID) != "trace-abc.1" {
		t.Errorf("incoming request ID not kept: %q", r.header.Get(HeaderRequestID))
	}
	r = e.do(req{method: "GET", path: "/healthz", as: "none", headers: map[string]string{HeaderRequestID: "bad id\n<script>"}})
	if got := r.header.Get(HeaderRequestID); got == "bad id\n<script>" || len(got) != 36 {
		t.Errorf("unsafe request ID echoed: %q", got)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/nope"}); r.status != 404 || r.errCode() != CodeNotFound {
		t.Errorf("unknown route: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "PUT", path: "/api/v1/tasks"}); r.status != 405 {
		t.Errorf("wrong method: %d", r.status)
	}
}

func TestRecovererPassesAbortThrough(t *testing.T) {
	h := recoverer(quiet)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if err, _ := recover().(error); !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler re-panicked", err)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestRecoverer(t *testing.T) {
	h := recoverer(quiet)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), CodeInternal) || strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("panic response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnexpectedErrorsAreNotLeaked(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, httptest.NewRequest("GET", "/", nil), quiet, io.ErrUnexpectedEOF)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "unexpected EOF") {
		t.Errorf("internal error: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writeError(rec, httptest.NewRequest("GET", "/", nil), quiet, agentpulse.ErrCircuitOpen)
	if rec.Code != 503 {
		t.Errorf("open circuit: %d", rec.Code)
	}
}

// --- operations ------------------------------------------------------------

func TestReadiness(t *testing.T) {
	var dbErr error
	e := newEnv(t, func(d *Deps) { d.Ready = func(context.Context) error { return dbErr } })
	if r := e.do(req{method: "GET", path: "/readyz", as: "none"}); r.status != 200 || r.json["status"] != "ready" {
		t.Errorf("ready: %d %s", r.status, r.raw)
	}
	dbErr = errors.New("connection refused to 10.0.0.5")
	r := e.do(req{method: "GET", path: "/readyz", as: "none"})
	if r.status != 503 || r.json["status"] != "not_ready" || strings.Contains(string(r.raw), "10.0.0.5") {
		t.Errorf("not ready: %d %s", r.status, r.raw)
	}
	if r := newEnv(t).do(req{method: "GET", path: "/readyz", as: "none"}); r.status != 200 {
		t.Errorf("no readiness check: %d", r.status)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	m := observability.NewMetrics(nil)
	e := newEnv(t, func(d *Deps) { d.Metrics = m })
	task := e.createTask(nil)
	e.do(req{method: "GET", path: "/api/v1/tasks/" + task["id"].(string)})
	e.do(req{method: "GET", path: "/api/v1/tasks/" + uuid.NewString()})
	e.do(req{method: "GET", path: "/no/such/" + uuid.NewString()})

	r := e.do(req{method: "GET", path: "/metrics", as: "none"})
	if r.status != 200 {
		t.Fatalf("metrics: %d", r.status)
	}
	body := string(r.raw)
	for _, want := range []string{
		`method="GET",route="/api/v1/tasks/{id}",status="200"`,
		`method="GET",route="/api/v1/tasks/{id}",status="404"`,
		`method="POST",route="/api/v1/tasks",status="201"`,
		`method="GET",route="unmatched",status="404"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics are missing %s", want)
		}
	}
	if strings.Contains(body, "/no/such") {
		t.Error("an unmatched path became a label value")
	}
	if r := newEnv(t).do(req{method: "GET", path: "/metrics", as: "none"}); r.status != 404 {
		t.Errorf("metrics without a registry: %d", r.status)
	}
}

// TestTraceReachesOutboxAndLogs: the trace of an incoming request is stored
// with its outbox rows, for the relay to continue, and logged.
func TestTraceReachesOutboxAndLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	e := newEnv(t, func(d *Deps) { d.Logger = logger })

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	r := e.do(req{method: "POST", path: "/api/v1/tasks", headers: map[string]string{
		"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01",
	}, body: map[string]any{"workspace_id": "ws_core", "title": "Traced", "assignee_id": "usr_carol"}})
	if r.status != 201 {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	entries, _ := e.b.Outbox.ListForAggregate(context.Background(), uuid.MustParse(r.json["id"].(string)))
	if len(entries) != 1 || !strings.Contains(entries[0].TraceContext["traceparent"], traceID) {
		t.Errorf("outbox trace context = %v", entries)
	}
	if !strings.Contains(logs.String(), `"trace_id":"`+traceID+`"`) {
		t.Errorf("access log lacks the trace ID: %s", logs.String())
	}
}

func TestEdgeCases(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	id := task["id"].(string)
	path := "/api/v1/tasks/" + id
	bad := "/api/v1/tasks/not-a-uuid"

	for name, tc := range map[string]struct {
		r      req
		status int
	}{
		"complete malformed ID":   {req{method: "POST", path: bad + "/complete"}, 404},
		"complete bad body":       {req{method: "POST", path: path + "/complete", body: `{"x":1}`}, 400},
		"complete bad If-Match":   {req{method: "POST", path: path + "/complete", headers: map[string]string{"If-Match": "abc"}}, 400},
		"restore malformed ID":    {req{method: "POST", path: bad + "/restore"}, 404},
		"comment malformed ID":    {req{method: "POST", path: bad + "/comments", body: map[string]any{"body": "x"}}, 404},
		"comment bad body":        {req{method: "POST", path: path + "/comments", body: `[]`}, 400},
		"feedback malformed ID":   {req{method: "POST", path: bad + "/feedback?signal=acted"}, 404},
		"feedback bad body":       {req{method: "POST", path: path + "/feedback", body: `{"signal":1}`}, 400},
		"update malformed ID":     {req{method: "PATCH", path: bad, body: map[string]any{"version": 1}}, 404},
		"update bad body":         {req{method: "PATCH", path: path, body: `nope`}, 400},
		"delete malformed ID":     {req{method: "DELETE", path: bad + "?permanent=true", as: "admin"}, 404},
		"rename bad body":         {req{method: "PATCH", path: "/api/v1/workspaces/ws_core/settings", as: "admin", body: `{`}, 400},
		"rename empty name":       {req{method: "PATCH", path: "/api/v1/workspaces/ws_core/settings", as: "admin", body: map[string]any{"name": " "}}, 422},
		"rename missing":          {req{method: "PATCH", path: "/api/v1/workspaces/nope/settings", as: "admin", body: map[string]any{"name": "x"}}, 404},
		"create workspace body":   {req{method: "POST", path: "/api/v1/workspaces", as: "admin", body: `{"id":`}, 400},
		"decision bad body":       {req{method: "POST", path: "/api/v1/admin/learning/suggestions/x/decision", as: "admin", body: `x`}, 400},
		"cursor without colon":    {req{method: "GET", path: "/api/v1/tasks?cursor=YWJj"}, 400},
		"cursor bad number":       {req{method: "GET", path: "/api/v1/tasks?cursor=eDp5"}, 400},
		"cursor bad uuid":         {req{method: "GET", path: "/api/v1/tasks?cursor=MTp5"}, 400},
		"oversized optional body": {req{method: "POST", path: path + "/complete", body: strings.Repeat(" ", maxBody+1)}, 413},
	} {
		if r := e.do(tc.r); r.status != tc.status {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}

	// A blocked task shows why; a reminder can be set and cleared.
	r := e.do(req{method: "PATCH", path: path, body: map[string]any{
		"version": 1, "status": "blocked", "remind_at": e.now.Add(time.Hour).Format(time.RFC3339),
	}})
	if r.status != 200 || r.json["blocked_reason"] != "manual" || r.json["remind_at"] == nil {
		t.Errorf("blocked with reminder: %d %s", r.status, r.raw)
	}

	// Completing from a notification still succeeds when feedback cannot be sent.
	e.do(req{method: "PATCH", path: path, body: map[string]any{"version": 2, "status": "todo"}})
	e.mock.FailNext("POST", "/v1/feedback", http.StatusServiceUnavailable, 1)
	if r := e.do(req{method: "POST", path: path + "/complete", as: "admin", body: map[string]any{"notification_id": "n"}}); r.status != 200 {
		t.Errorf("complete with feedback down: %d %s", r.status, r.raw)
	}

	if (&httpError{message: "m"}).Error() != "m" {
		t.Error("httpError.Error")
	}
}

func TestRateLimiterDefaultsAndSweep(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{})
	if rl.limit != 20 || rl.burst != 40 {
		t.Errorf("defaults = %v/%d", rl.limit, rl.burst)
	}
	rl.allow("a")
	rl.allow("b")
	// Pretend both buckets have been idle, and a sweep is due.
	for _, b := range rl.buckets {
		b.seen = time.Now().Add(-2 * idleBucket)
	}
	rl.lastSweep = time.Now().Add(-2 * time.Minute)
	rl.allow("c")
	if _, ok := rl.buckets["a"]; ok || len(rl.buckets) != 1 {
		t.Errorf("idle buckets were not swept: %d left", len(rl.buckets))
	}
}
