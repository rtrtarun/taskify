package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/worker"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

func feedItem(id, title string, payload map[string]any) mockagentpulse.FeedItem {
	return mockagentpulse.FeedItem{"id": id, "eventType": "task.updated", "title": title, "body": "<b>b</b>",
		"createdAt": "2026-11-20T10:00:00Z", "payload": payload}
}

const feedPath = "/v1/tenants/tenant_acme/users/usr_bob/in-app-feed"

// TestInAppFeed is M14-06: the feed is the caller's own, enriched with the
// caller's tasks and read state, and nothing in the request can change whose.
func TestInAppFeed(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(map[string]any{"title": "Fix checkout", "labels": []string{"bug"}})
	e.mock.SeedFeed("tenant_acme", "usr_bob",
		feedItem("ntf-1", "Updated", map[string]any{"taskId": task["id"], "jobId": "0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b"}),
		feedItem("ntf-2", "Foreign task", map[string]any{"taskId": "0193a1b2-c3d4-7e5f-8a6b-000000000000"}),
		feedItem("ntf-3", "No task", map[string]any{}))
	e.mock.SeedFeed("tenant_acme", "usr_eve", feedItem("ntf-eve", "Eve's secret", nil))
	e.mock.SeedFeed("tenant_other", "usr_bob", feedItem("ntf-other", "Other tenant's secret", nil))

	r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app"})
	items, _ := r.json["notifications"].([]any)
	if r.status != 200 || len(items) != 3 {
		t.Fatalf("feed: %d %s", r.status, r.raw)
	}
	first := items[0].(map[string]any)
	tk, _ := first["task"].(map[string]any)
	if first["id"] != "ntf-1" || first["task_id"] != task["id"] || tk["title"] != "Fix checkout" || first["read"] != false ||
		first["job_id"] == nil || first["body"] != "<b>b</b>" {
		t.Errorf("first item: %v", first)
	}
	if items[1].(map[string]any)["task"] != nil || items[2].(map[string]any)["task_id"] != nil {
		t.Errorf("unenriched items: %v %v", items[1], items[2])
	}

	// Trying to name someone else changes nothing: AgentPulse is only ever
	// asked for the token's tenant and user.
	for _, q := range []string{"?user_id=usr_eve", "?tenant_id=tenant_other&user_id=usr_eve", "?userId=usr_eve"} {
		r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app" + q, headers: map[string]string{
			HeaderTenantID: "tenant_other", HeaderUserID: "usr_eve", "X-User-ID-Override": "usr_eve"}})
		if r.status != 200 || len(r.json["notifications"].([]any)) != 3 {
			t.Errorf("%s: %d %s", q, r.status, r.raw)
		}
	}
	for _, rq := range e.mock.Requests() {
		if rq.Path != feedPath {
			t.Errorf("AgentPulse was asked for %s", rq.Path)
		}
	}
	if r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app", as: "other-tenant"}); len(r.json["notifications"].([]any)) != 0 {
		t.Errorf("another tenant's user sees %s", r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app", as: "none"}); r.status != 401 {
		t.Errorf("no credentials: %d", r.status)
	}

	// Paging passes through; read state is per person.
	r = e.do(req{method: "GET", path: "/api/v1/notifications/in-app?limit=2"})
	cursor, _ := r.json["next_cursor"].(string)
	if len(r.json["notifications"].([]any)) != 2 || cursor == "" {
		t.Fatalf("page 1: %s", r.raw)
	}
	r = e.do(req{method: "GET", path: "/api/v1/notifications/in-app?limit=2&cursor=" + cursor})
	if len(r.json["notifications"].([]any)) != 1 || r.json["next_cursor"] != nil {
		t.Errorf("page 2: %s", r.raw)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=x"} {
		if r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app?" + q}); r.status != 400 {
			t.Errorf("%s: %d", q, r.status)
		}
	}

	if r := e.do(req{method: "POST", path: "/api/v1/notifications/ntf-1/ack", body: map[string]any{"signal": "snoozed", "duration": "2h"}}); r.status != 200 {
		t.Fatalf("ack: %d %s", r.status, r.raw)
	}
	r = e.do(req{method: "GET", path: "/api/v1/notifications/in-app"})
	if got := r.json["notifications"].([]any)[0].(map[string]any); got["read"] != true || got["signal"] != "snoozed" || got["snoozed_until"] == nil {
		t.Errorf("after ack: %v", got)
	}
}

// TestInAppFeedUnavailable: AgentPulse failing is a 503 with a retry hint,
// and task reads keep working.
func TestInAppFeedUnavailable(t *testing.T) {
	e := newEnv(t)
	task := e.createTask(nil)
	e.mock.FailNext("GET", feedPath, 500, -1)
	r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app"})
	if r.status != 503 || r.header.Get("Retry-After") == "" || r.errCode() != CodeUpstreamUnavailable {
		t.Fatalf("feed down: %d %v %s", r.status, r.header, r.raw)
	}
	if r := e.do(req{method: "GET", path: "/api/v1/tasks/" + task["id"].(string)}); r.status != 200 {
		t.Errorf("task read with AgentPulse down: %d", r.status)
	}
	e.mock.FailNext("GET", feedPath, 400, -1)
	if r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app"}); r.status != 502 {
		t.Errorf("a refusal from AgentPulse: %d %s", r.status, r.raw)
	}
	e.mock.ClearFailures()
	if r := e.do(req{method: "GET", path: "/api/v1/notifications/in-app"}); r.status != 200 {
		t.Errorf("recovered: %d", r.status)
	}
}

func (e *env) feedbackRows() []domain.OutboxEntry {
	e.t.Helper()
	rows, err := e.b.Outbox.ListForAggregate(context.Background(), domain.FeedbackAggregateID(tenant, "usr_bob", "ntf-1"))
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// TestAckNotification: the ack is saved with one outbox row; repeating it
// adds none; a different reaction adds one; and a failed first send is
// retried to exactly one feedback call.
func TestAckNotification(t *testing.T) {
	e := newEnv(t)
	e.mock.SeedNotification("ntf-1", map[string]any{"tenantId": tenant, "userId": "usr_bob"})
	ack := func(body any) resp {
		return e.do(req{method: "POST", path: "/api/v1/notifications/ntf-1/ack", body: body})
	}

	r := ack(map[string]any{"signal": "acted"})
	if r.status != 200 || r.json["signal"] != "acted" || r.json["snoozed_until"] != nil {
		t.Fatalf("ack: %d %s", r.status, r.raw)
	}
	rows := e.feedbackRows()
	var f domain.FeedbackForward
	if len(rows) != 1 || rows[0].EventType != domain.EventFeedbackForward || json.Unmarshal(rows[0].Payload, &f) != nil ||
		f.UserID != "usr_bob" || f.NotificationID != "ntf-1" || f.Signal != "acted" {
		t.Fatalf("outbox = %+v", rows)
	}
	first := r.json["acked_at"]
	e.now = e.now.Add(time.Minute)
	if r := ack(map[string]any{"signal": "acted"}); r.status != 200 || r.json["acked_at"] != first || len(e.feedbackRows()) != 1 {
		t.Errorf("repeat: %d %s rows=%d", r.status, r.raw, len(e.feedbackRows()))
	}
	if r := ack(map[string]any{"signal": "dismissed"}); r.status != 200 || len(e.feedbackRows()) != 2 {
		t.Errorf("a different reaction: %d rows=%d", r.status, len(e.feedbackRows()))
	}

	// The first send fails; the relay retries; AgentPulse sees one call per row.
	e.mock.FailNext("POST", "/v1/feedback", 500, 1)
	relay := worker.NewRelay(e.b.Outbox, agentpulse.New(e.apURL, "k", agentpulse.WithBreaker(nil)),
		worker.RelayConfig{Now: func() time.Time { return e.now.Add(time.Hour) }, Backoff: func(int) time.Duration { return 0 }, Logger: quiet})
	for i := 0; i < 4; i++ {
		if _, err := relay.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	sent := e.mock.Feedback()
	if len(sent) != 2 || sent[0]["notificationId"] != "ntf-1" || sent[0]["userId"] != "usr_bob" || sent[0]["tenantId"] != tenant {
		t.Errorf("AgentPulse feedback = %v (one call per reaction, even after a failed attempt)", sent)
	}

	for name, body := range map[string]any{
		"bad signal":          map[string]any{"signal": "muted"},
		"snooze, no duration": map[string]any{"signal": "snoozed"},
		"duration on acted":   map[string]any{"signal": "acted", "duration": "2h"},
		"too long a snooze":   map[string]any{"signal": "snoozed", "duration": "999h"},
	} {
		if r := ack(body); r.status != 422 {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	for name, body := range map[string]any{"unknown field": map[string]any{"signal": "acted", "user_id": "usr_eve"}, "empty": ""} {
		if r := ack(body); r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	if r := e.do(req{method: "POST", path: "/api/v1/notifications/ntf-1/ack", as: "none", body: map[string]any{"signal": "acted"}}); r.status != 401 {
		t.Errorf("no credentials: %d", r.status)
	}
	// Another person's ack of the same ID is theirs alone.
	if r := e.do(req{method: "POST", path: "/api/v1/notifications/ntf-1/ack", as: "admin", body: map[string]any{"signal": "acted"}}); r.status != 200 {
		t.Fatalf("admin ack: %d", r.status)
	}
	if rows := e.feedbackRows(); len(rows) != 2 {
		t.Errorf("another user's ack touched bob's rows: %d", len(rows))
	}
	_ = http.StatusOK
}
