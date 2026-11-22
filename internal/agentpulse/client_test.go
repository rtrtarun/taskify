package agentpulse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

func newMock(t *testing.T) (*mockagentpulse.Server, string) {
	t.Helper()
	m := mockagentpulse.New()
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return m, ts.URL
}

func decode(t *testing.T, r mockagentpulse.Request) map[string]any {
	t.Helper()
	var v map[string]any
	if err := r.JSON(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNotifySendsBodyAndHeaders(t *testing.T) {
	mock, url := newMock(t)
	c := New(url+"/", "secret-key")

	body := json.RawMessage(`{"idempotencyKey":"k1","tenantId":"t","userId":"u","eventType":"task.created","title":"x","body":"y","payload":{"bypassDigest":true}}`)
	// A context carrying a trace, as the relay builds from an outbox row.
	ctx := observability.ExtractTraceContext(context.Background(),
		map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	if err := c.Notify(ctx, body); err != nil {
		t.Fatal(err)
	}
	reqs := mock.RequestsTo("POST", "/v1/notify")
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if string(r.Body) != string(body) {
		t.Errorf("body was modified:\n got %s\nwant %s", r.Body, body)
	}
	if r.Header.Get("Authorization") != "Bearer secret-key" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", r.Header)
	}
	if tp := r.Header.Get("traceparent"); !strings.HasPrefix(tp, "00-0af7651916cd43dd8448eb211c80319c-") {
		t.Errorf("traceparent = %q, want the same trace ID", tp)
	}
	// A duplicate is acknowledged with 200 and is a success.
	if err := c.Notify(context.Background(), body); err != nil {
		t.Errorf("duplicate notify: %v", err)
	}
}

func TestNotifyRewritesLegacyBypassKey(t *testing.T) {
	mock, url := newMock(t)
	c := New(url, "k")
	body := json.RawMessage(`{"idempotencyKey":"k1","tenantId":"t","userId":"u","eventType":"e","title":"x","body":"y","payload":{"bypass_digest":true,"taskId":"1"}}`)
	if err := c.Notify(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	p := decode(t, mock.RequestsTo("POST", "/v1/notify")[0])["payload"].(map[string]any)
	if p["bypassDigest"] != true || p["bypass_digest"] != nil || p["taskId"] != "1" {
		t.Errorf("payload = %v", p)
	}
	if err := c.Notify(context.Background(), json.RawMessage(`{"bypass_digest": `)); err == nil {
		t.Error("malformed body was accepted")
	}
}

func TestOtherCalls(t *testing.T) {
	mock, url := newMock(t)
	c := New(url, "k")
	ctx := context.Background()

	// last returns the only request made since the previous call, checking
	// its method, path, query, and exact body.
	seen := 0
	last := func(method, path, query, body string) {
		t.Helper()
		reqs := mock.Requests()
		if len(reqs) != seen+1 {
			t.Fatalf("%s %s: %d new requests, want 1", method, path, len(reqs)-seen)
		}
		r := reqs[seen]
		seen++
		if r.Method != method || r.Path != path || r.Query != query {
			t.Errorf("request = %s %s?%s, want %s %s?%s", r.Method, r.Path, r.Query, method, path, query)
		}
		if strings.TrimSpace(string(r.Body)) != body {
			t.Errorf("%s %s body:\n got %s\nwant %s", method, path, r.Body, body)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("%s %s: Authorization = %q", method, path, r.Header.Get("Authorization"))
		}
	}

	if err := c.RecordFeedback(ctx, Feedback{NotificationID: "n1", TenantID: "t", UserID: "u", Signal: "snoozed", SnoozeDuration: "2h"}); err != nil {
		t.Fatal(err)
	}
	last("POST", "/v1/feedback", "", `{"notificationId":"n1","tenantId":"t","userId":"u","signal":"snoozed","snoozeDuration":"2h"}`)
	if err := c.RecordFeedback(ctx, Feedback{TenantID: "t", UserID: "u", Signal: "shrug"}); err == nil {
		t.Error("an invalid signal was accepted")
	}
	seen++

	id, err := c.CreateRule(ctx, Rule{TenantID: "t", Name: "low", MatchAny: []RuleMatch{{PayloadMatch: map[string]any{"priority": "p4_low"}}}, Channels: []string{"digest"}})
	if err != nil || id == "" {
		t.Fatalf("CreateRule = %q, %v", id, err)
	}
	last("POST", "/v1/rules", "", `{"tenantId":"t","name":"low","matchAny":[{"payloadMatch":{"priority":"p4_low"}}],"channels":["digest"]}`)

	raw, err := c.ListRules(ctx, "t")
	if err != nil || !strings.Contains(string(raw), `"id":"`+id+`"`) {
		t.Fatalf("ListRules = %s, %v", raw, err)
	}
	last("GET", "/v1/rules", "tenantId=t", "")

	sid := mock.SeedSuggestion(map[string]any{"tenantId": "t.a-1", "proposedAdjustment": 5.0})
	raw, err = c.ListLearningSuggestions(ctx, "t.a-1")
	if err != nil || !strings.Contains(string(raw), sid) {
		t.Fatalf("ListLearningSuggestions = %s, %v", raw, err)
	}
	last("GET", "/v1/learning/suggestions", "tenantId=t.a-1", "")
	raw, err = c.ReviewLearningSuggestion(ctx, sid, "approve", "usr_admin")
	if err != nil || !strings.Contains(string(raw), "APPROVED") {
		t.Fatalf("ReviewLearningSuggestion = %s, %v", raw, err)
	}
	last("POST", "/v1/learning/suggestions/"+sid+"/decision", "", `{"decidedBy":"usr_admin","decision":"approve"}`)
	if _, err := c.ReviewLearningSuggestion(ctx, "missing", "approve", "u"); err == nil {
		t.Error("unknown suggestion did not fail")
	}
	seen++

	mock.SeedFeed("t", "usr 1", mockagentpulse.FeedItem{"id": "n2"}, mockagentpulse.FeedItem{"id": "n1"})
	raw, err = c.InAppFeed(ctx, "t", "usr 1", 1, "")
	if err != nil || !strings.Contains(string(raw), `"n2"`) || !strings.Contains(string(raw), `"nextCursor":"1"`) {
		t.Fatalf("InAppFeed = %s, %v", raw, err)
	}
	last("GET", "/v1/tenants/t/users/usr 1/in-app-feed", "limit=1", "")
	raw, err = c.InAppFeed(ctx, "t", "usr 1", 0, "1")
	if err != nil || !strings.Contains(string(raw), `"n1"`) {
		t.Fatalf("InAppFeed next page = %s, %v", raw, err)
	}
	last("GET", "/v1/tenants/t/users/usr 1/in-app-feed", "cursor=1", "")
}

func TestErrorClassification(t *testing.T) {
	mock, url := newMock(t)
	c := New(url, "k", WithBreaker(nil))
	body := json.RawMessage(`{"idempotencyKey":"k","tenantId":"t","userId":"u","eventType":"e","title":"x","body":"y"}`)

	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusServiceUnavailable, true},
		{http.StatusInternalServerError, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusUnprocessableEntity, false},
	} {
		mock.FailNext("POST", "/v1/notify", tc.status, 1)
		err := c.Notify(context.Background(), body)
		var se *StatusError
		if !errors.As(err, &se) || se.Code != tc.status {
			t.Fatalf("status %d: error = %v", tc.status, err)
		}
		if Retryable(err) != tc.retryable {
			t.Errorf("status %d: Retryable = %v, want %v", tc.status, Retryable(err), tc.retryable)
		}
		if !strings.Contains(err.Error(), "scripted failure") || strings.Contains(err.Error(), "k\"") {
			t.Errorf("error text = %q", err)
		}
	}

	if !IsUpstream(ErrCircuitOpen) || IsUpstream(errors.New("local bug")) || IsUpstream(nil) {
		t.Error("IsUpstream misclassifies")
	}
	if Retryable(nil) {
		t.Error("nil error is retryable")
	}
	if !Retryable(ErrCircuitOpen) {
		t.Error("an open circuit must be retryable")
	}

	unreachable := New("http://127.0.0.1:1", "k", WithBreaker(nil))
	err := unreachable.Notify(context.Background(), body)
	if err == nil || !Retryable(err) {
		t.Errorf("connection refused: error = %v, retryable %v", err, Retryable(err))
	}
	if !IsUpstream(err) {
		t.Errorf("connection refused is not upstream: %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()
	timeout := New(slow.URL, "k", WithBreaker(nil), WithHTTPClient(&http.Client{Timeout: 20 * time.Millisecond}))
	if err := timeout.Notify(context.Background(), body); err == nil || !Retryable(err) {
		t.Errorf("timeout: error = %v", err)
	}
}

func TestClientWithBreaker(t *testing.T) {
	mock, url := newMock(t)
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	c := New(url, "k", WithBreaker(NewBreaker(20, 0.5, 10*time.Second).WithClock(clock)))
	body := json.RawMessage(`{"idempotencyKey":"k","tenantId":"t","userId":"u","eventType":"e","title":"x","body":"y"}`)

	mock.FailNext("POST", "/v1/notify", http.StatusServiceUnavailable, -1)
	for i := 0; i < 20; i++ {
		_ = c.Notify(context.Background(), body)
	}
	if c.BreakerState() != BreakerOpen {
		t.Fatalf("state = %s, want open after 20 failures", c.BreakerState())
	}
	sent := len(mock.Requests())
	if err := c.Notify(context.Background(), body); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("error = %v, want ErrCircuitOpen", err)
	}
	if len(mock.Requests()) != sent {
		t.Error("a request reached AgentPulse while the circuit was open")
	}

	mock.ClearFailures()
	now = now.Add(10 * time.Second)
	if err := c.Notify(context.Background(), body); err != nil {
		t.Fatalf("trial request: %v", err)
	}
	if c.BreakerState() != BreakerClosed {
		t.Errorf("state = %s, want closed after a successful trial", c.BreakerState())
	}

	// 4xx responses never open the breaker.
	mock.FailNext("POST", "/v1/notify", http.StatusBadRequest, -1)
	for i := 0; i < 30; i++ {
		_ = c.Notify(context.Background(), body)
	}
	if c.BreakerState() != BreakerClosed {
		t.Errorf("state = %s after 4xx responses, want closed", c.BreakerState())
	}
	if New(url, "k", WithBreaker(nil)).BreakerState() != BreakerClosed {
		t.Error("a client without a breaker must report closed")
	}
}

func TestBreaker(t *testing.T) {
	now := time.Unix(0, 0)
	newB := func() *Breaker {
		return NewBreaker(20, 0.5, 10*time.Second).WithClock(func() time.Time { return now })
	}
	record := func(b *Breaker, failures, successes int) {
		for i := 0; i < failures; i++ {
			b.Allow()
			b.Record(true)
		}
		for i := 0; i < successes; i++ {
			b.Allow()
			b.Record(false)
		}
	}

	t.Run("stays closed at exactly 50 percent", func(t *testing.T) {
		b := newB()
		record(b, 10, 10)
		if b.State() != BreakerClosed {
			t.Errorf("state = %s", b.State())
		}
	})

	t.Run("stays closed until the window is full", func(t *testing.T) {
		b := newB()
		record(b, 19, 0)
		if b.State() != BreakerClosed {
			t.Errorf("state = %s after 19 requests", b.State())
		}
		record(b, 1, 0)
		if b.State() != BreakerOpen {
			t.Errorf("state = %s after 20 failures", b.State())
		}
	})

	t.Run("opens above 50 percent and the window rolls", func(t *testing.T) {
		b := newB()
		record(b, 0, 10)
		record(b, 10, 0) // 10 of 20: closed
		if b.State() != BreakerClosed {
			t.Fatalf("state = %s", b.State())
		}
		record(b, 1, 0) // the oldest success rolls out: 11 of 20
		if b.State() != BreakerOpen {
			t.Errorf("state = %s, want open", b.State())
		}
	})

	t.Run("cooldown, one trial, then close or reopen", func(t *testing.T) {
		now = time.Unix(0, 0)
		b := newB()
		record(b, 20, 0)
		if b.Allow() {
			t.Fatal("allowed while open")
		}
		now = now.Add(9 * time.Second)
		if b.Allow() {
			t.Fatal("allowed before the cooldown ended")
		}
		now = now.Add(time.Second)
		if b.State() != BreakerHalfOpen {
			t.Errorf("state = %s, want half_open", b.State())
		}
		if !b.Allow() {
			t.Fatal("trial not allowed after the cooldown")
		}
		if b.Allow() {
			t.Fatal("a second concurrent trial was allowed")
		}
		b.Record(true)
		if b.State() != BreakerOpen || b.Allow() {
			t.Fatalf("failed trial: state = %s", b.State())
		}
		now = now.Add(10 * time.Second)
		b.Allow()
		b.Record(false)
		if b.State() != BreakerClosed {
			t.Errorf("successful trial: state = %s", b.State())
		}
		// The window was reset: 19 fresh failures do not open it.
		record(b, 19, 0)
		if b.State() != BreakerClosed {
			t.Errorf("state = %s, want closed with a fresh window", b.State())
		}
	})

	t.Run("results arriving after it opened are ignored", func(t *testing.T) {
		b := newB()
		record(b, 20, 0)
		b.Record(false)
		if b.State() != BreakerOpen {
			t.Errorf("state = %s", b.State())
		}
	})

	t.Run("safe for concurrent use", func(t *testing.T) {
		b := DefaultBreaker()
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					if b.Allow() {
						b.Record(i%3 == g%3)
					}
					_ = b.State()
				}
			}(g)
		}
		wg.Wait()
	})
}
