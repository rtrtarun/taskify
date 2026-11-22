// Package e2e runs the assembled service against PostgreSQL and a mock
// AgentPulse, with its background workers running: a request goes in, and
// the notification comes out the other side.
//
// It needs DATABASE_URL and is skipped without it.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rtrtarun/taskify/internal/api"
	"github.com/rtrtarun/taskify/internal/app"
	"github.com/rtrtarun/taskify/internal/config"
	"github.com/rtrtarun/taskify/internal/repository/postgres"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

const (
	jwtSecret     = "e2e-jwt-secret"
	webhookSecret = "e2e-webhook-secret"
	tenant        = "tenant_e2e"
)

type harness struct {
	t    *testing.T
	url  string
	mock *mockagentpulse.Server
	db   *postgres.DB
}

func start(t *testing.T) *harness {
	t.Helper()
	db := repotest.NewDatabase(t) // skips without DATABASE_URL

	mock := mockagentpulse.New()
	apSrv := httptest.NewServer(mock)
	t.Cleanup(apSrv.Close)

	cfg := testConfig(apSrv.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.NewWithDB(db, cfg, logger, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.RunWorkers(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("workers: %v", err)
		}
	})
	return &harness{t: t, url: srv.URL, mock: mock, db: db}
}

func testConfig(agentPulseURL string) config.Config {
	return config.Config{
		Env: config.EnvProduction, PublicBaseURL: "https://taskify.e2e",
		AgentPulseBaseURL: agentPulseURL, AgentPulseAPIKey: "e2e-key", AgentPulseWebhookSecret: webhookSecret,
		OutboxWorkers: 4, OutboxBatchSize: 50, OutboxPollInterval: 20 * time.Millisecond,
		OutboxMaxAttempts: 10, OutboxLease: time.Minute, SchedulerInterval: time.Hour,
		JWTAlgorithm: config.JWTAlgHS256, JWTSecret: jwtSecret, RateLimitRPS: 1000, RateLimitBurst: 1000,
	}
}

// notifyHeader returns a header of the first /notify request for eventType.
func (h *harness) notifyHeader(eventType, header string) string {
	for _, r := range h.mock.RequestsTo("POST", "/v1/notify") {
		var body map[string]any
		if r.JSON(&body) == nil && body["eventType"] == eventType {
			return r.Header.Get(header)
		}
	}
	return ""
}

func (h *harness) token(user, role string) string {
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": user, "tenant_id": tenant, "role": role, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(jwtSecret))
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) call(method, path, user, role string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if raw, ok := body.([]byte); ok {
			buf.Write(raw)
		} else {
			_ = json.NewEncoder(&buf).Encode(body)
		}
	}
	req, err := http.NewRequest(method, h.url+path, &buf)
	if err != nil {
		h.t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+h.token(user, role))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// waitForNotify waits until AgentPulse has accepted a notification matching
// eventType and user, and returns its body.
func (h *harness) waitForNotify(eventType, user string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range h.mock.Notifications() {
			if n.Body["eventType"] == eventType && n.Body["userId"] == user {
				return n.Body
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("no %s notification for %s reached AgentPulse", eventType, user)
	return nil
}

// TestEndToEnd is the M3 exit criterion: create a task and see /notify
// arrive with the expected payload, forward feedback, approve a suggestion,
// and complete the task from a notification.
func TestEndToEnd(t *testing.T) {
	h := start(t)

	// An admin creates the workspace; its default rules reach AgentPulse.
	status, ws := h.call("POST", "/api/v1/workspaces", "usr_admin", "admin", map[string]any{"id": "ws_e2e", "name": "E2E"}, nil)
	if status != 201 || ws["rules_bootstrapped"] != true {
		t.Fatalf("create workspace: %d %v", status, ws)
	}
	if n := len(h.mock.Rules()); n != 3 {
		t.Fatalf("AgentPulse has %d rules, want 3", n)
	}

	// A member creates a critical task for someone else, inside a trace
	// their client started.
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	status, task := h.call("POST", "/api/v1/tasks", "usr_bob", "member", map[string]any{
		"workspace_id": "ws_e2e", "title": "Checkout is down", "priority": "p1_critical", "assignee_id": "usr_carol",
	}, map[string]string{"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01"})
	if status != 201 {
		t.Fatalf("create task: %d %v", status, task)
	}
	id := task["id"].(string)

	// The relay delivers it, without the API having called AgentPulse.
	n := h.waitForNotify("task.created", "usr_carol")
	if n["idempotencyKey"] != "taskify:"+tenant+":"+id+":task.created:v1" || n["tenantId"] != tenant ||
		n["source"] != "taskify" || n["priorityHint"] != "critical" || n["title"] != "New task: Checkout is down" {
		t.Errorf("notification envelope = %v", n)
	}
	payload := n["payload"].(map[string]any)
	if payload["taskId"] != id || payload["workspaceId"] != "ws_e2e" || payload["bypassDigest"] != true || payload["actorId"] != "usr_bob" {
		t.Errorf("notification payload = %v", payload)
	}
	if got := h.mock.RequestsTo("POST", "/v1/notify")[0].Header.Get("Authorization"); got != "Bearer e2e-key" {
		t.Errorf("AgentPulse auth header = %q", got)
	}
	// One trace from the client's request to the AgentPulse call.
	if tp := h.notifyHeader("task.created", "traceparent"); !strings.HasPrefix(tp, "00-"+traceID+"-") {
		t.Errorf("AgentPulse traceparent = %q, want trace %s", tp, traceID)
	}

	// The assignee dismisses it; the signal reaches AgentPulse.
	if status, _ := h.call("POST", "/api/v1/tasks/"+id+"/feedback", "usr_carol", "member", map[string]any{"signal": "dismissed", "notification_id": "ntf_1"}, nil); status != 202 {
		t.Fatalf("feedback: %d", status)
	}
	if fb := h.mock.Feedback(); len(fb) != 1 || fb[0]["signal"] != "dismissed" || fb[0]["userId"] != "usr_carol" {
		t.Errorf("feedback at AgentPulse = %v", fb)
	}

	// An admin approves a learning suggestion through the proxy.
	sid := h.mock.SeedSuggestion(map[string]any{"tenantId": tenant, "proposedAdjustment": -10.0, "signalCount": 6.0})
	status, list := h.call("GET", "/api/v1/admin/learning/suggestions", "usr_admin", "admin", nil, nil)
	if status != 200 || len(list["suggestions"].([]any)) != 1 {
		t.Fatalf("list suggestions: %d %v", status, list)
	}
	if status, out := h.call("POST", "/api/v1/admin/learning/suggestions/"+sid+"/decision", "usr_admin", "admin", map[string]any{"decision": "approve"}, nil); status != 200 || out["status"] != "APPROVED" {
		t.Fatalf("approve: %d %v", status, out)
	}

	// The assignee clicks "Mark done" in Slack: AgentPulse calls the webhook.
	raw, _ := json.Marshal(map[string]any{"action": "complete", "tenantId": tenant, "userId": "usr_carol", "taskId": id, "notificationId": "ntf_1"})
	ts := time.Now().Unix()
	status, out := h.call("POST", "/api/v1/webhooks/agentpulse", "", "", raw, map[string]string{
		api.HeaderWebhookTimestamp: strconv.FormatInt(ts, 10),
		api.HeaderWebhookSignature: api.SignWebhook(webhookSecret, ts, raw),
	})
	if status != 200 || out["status"] != "applied" {
		t.Fatalf("webhook: %d %v", status, out)
	}
	if _, got := h.call("GET", "/api/v1/tasks/"+id, "usr_bob", "member", nil, nil); got["status"] != "completed" {
		t.Errorf("task status after webhook = %v", got["status"])
	}
	// The creator hears about it, and the click became an acted signal.
	h.waitForNotify("task.completed", "usr_bob")
	if fb := h.mock.Feedback(); len(fb) != 2 || fb[1]["signal"] != "acted" {
		t.Errorf("feedback at AgentPulse = %v", fb)
	}
	if h.mock.Duplicates() != 0 {
		t.Errorf("AgentPulse saw %d duplicate notifications", h.mock.Duplicates())
	}
}

// TestPermanentDeleteStopsNotifications: once deleted, nothing about the
// task is sent, even if it was still waiting in the outbox.
func TestPermanentDeleteStopsNotifications(t *testing.T) {
	h := start(t)
	h.call("POST", "/api/v1/workspaces", "usr_admin", "admin", map[string]any{"id": "ws_e2e", "name": "E2E"}, nil)

	// With AgentPulse failing, notifications wait in the outbox.
	h.mock.FailNext("POST", "/v1/notify", http.StatusServiceUnavailable, -1)
	_, task := h.call("POST", "/api/v1/tasks", "usr_bob", "member", map[string]any{
		"workspace_id": "ws_e2e", "title": "Contains personal data", "assignee_id": "usr_carol",
	}, nil)
	id := task["id"].(string)
	if status, _ := h.call("DELETE", "/api/v1/tasks/"+id+"?permanent=true", "usr_admin", "admin", nil, nil); status != 204 {
		t.Fatalf("permanent delete: %d", status)
	}
	h.mock.ClearFailures()
	time.Sleep(500 * time.Millisecond) // several relay polls
	for _, n := range h.mock.Notifications() {
		if p, _ := n.Body["payload"].(map[string]any); p["taskId"] == id {
			t.Fatalf("a notification for the deleted task was delivered: %v", n.Body)
		}
	}
}

// TestContactReachesAgentPulse: a person's contact goes through the outbox
// to AgentPulse's contact endpoint as a full replace (AP-05).
func TestContactReachesAgentPulse(t *testing.T) {
	h := start(t)
	status, out := h.call("PUT", "/api/v1/me/contact", "usr_carol", "member",
		map[string]any{"email": "carol@example.com", "slack_user_id": "U024BE7LH"}, nil)
	if status != 200 || out["email"] != "carol@example.com" {
		t.Fatalf("put contact: %d %v", status, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, ok := h.mock.ContactOf(tenant, "usr_carol"); ok && c.Email == "carol@example.com" && c.SlackUserID == "U024BE7LH" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the contact never reached AgentPulse: %v", h.mock.Requests())
		}
		time.Sleep(20 * time.Millisecond)
	}
	reqs := h.mock.RequestsTo("PUT", "/v1/tenants/"+tenant+"/users/usr_carol/contact")
	if len(reqs) != 1 || string(reqs[0].Body) != `{"email":"carol@example.com","slackUserId":"U024BE7LH","phone":null}` {
		t.Errorf("PUTs = %d", len(reqs))
	}
	if status, out := h.call("GET", "/api/v1/me/contact", "usr_carol", "member", nil, nil); status != 200 || out["slack_user_id"] != "U024BE7LH" {
		t.Errorf("get contact: %d %v", status, out)
	}
}
