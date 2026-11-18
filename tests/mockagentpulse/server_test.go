package mockagentpulse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func start(t *testing.T) (*Server, string) {
	t.Helper()
	s := New()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func do(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-abc-def-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func notifyBody(key string) map[string]any {
	return map[string]any{
		"idempotencyKey": key,
		"tenantId":       "tenant_acme",
		"userId":         "usr_1",
		"eventType":      "task.blocked",
		"title":          "t",
		"body":           "b",
	}
}

func TestNotifyDeduplicatesOnIdempotencyKey(t *testing.T) {
	s, url := start(t)

	status, first := do(t, "POST", url+"/notify", notifyBody("k1"))
	if status != http.StatusAccepted || first["duplicate"] != false {
		t.Fatalf("first call: status %d body %v", status, first)
	}
	status, second := do(t, "POST", url+"/notify", notifyBody("k1"))
	if status != http.StatusOK || second["duplicate"] != true || second["id"] != first["id"] {
		t.Fatalf("second call: status %d body %v", status, second)
	}
	do(t, "POST", url+"/notify", notifyBody("k2"))

	if got := len(s.Notifications()); got != 2 {
		t.Errorf("accepted notifications = %d, want 2", got)
	}
	if got := s.Duplicates(); got != 1 {
		t.Errorf("duplicates = %d, want 1", got)
	}
	if got := len(s.RequestsTo("POST", "/notify")); got != 3 {
		t.Errorf("recorded /notify requests = %d, want 3", got)
	}
}

func TestNotifyDedupUnderConcurrency(t *testing.T) {
	s, url := start(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			do(t, "POST", url+"/notify", notifyBody(fmt.Sprintf("k%d", i%10)))
		}(i)
	}
	wg.Wait()
	if got := len(s.Notifications()); got != 10 {
		t.Errorf("accepted notifications = %d, want 10", got)
	}
	if got := s.Duplicates(); got != 30 {
		t.Errorf("duplicates = %d, want 30", got)
	}
}

func TestNotifyRejectsMissingField(t *testing.T) {
	s, url := start(t)
	body := notifyBody("k1")
	delete(body, "userId")
	if status, _ := do(t, "POST", url+"/notify", body); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if len(s.Notifications()) != 0 {
		t.Error("an invalid notification was accepted")
	}
}

func TestRequestsAreRecorded(t *testing.T) {
	s, url := start(t)
	do(t, "POST", url+"/notify", notifyBody("k1"))
	do(t, "GET", url+"/healthz", nil)

	reqs := s.Requests()
	if len(reqs) != 1 {
		t.Fatalf("recorded = %d, want 1 (health checks are not recorded)", len(reqs))
	}
	if reqs[0].Header.Get("traceparent") != "00-abc-def-01" {
		t.Errorf("traceparent header not recorded: %v", reqs[0].Header)
	}
	var got map[string]any
	if err := reqs[0].JSON(&got); err != nil || got["idempotencyKey"] != "k1" {
		t.Errorf("recorded body = %v, err %v", got, err)
	}
}

func TestScriptedFailures(t *testing.T) {
	s, url := start(t)
	s.FailNext("POST", "/notify", http.StatusServiceUnavailable, 3)

	for i := 0; i < 3; i++ {
		if status, _ := do(t, "POST", url+"/notify", notifyBody("k1")); status != http.StatusServiceUnavailable {
			t.Fatalf("call %d: status = %d, want 503", i+1, status)
		}
	}
	if len(s.Notifications()) != 0 {
		t.Fatal("a failed call was accepted")
	}
	if status, _ := do(t, "POST", url+"/notify", notifyBody("k1")); status != http.StatusAccepted {
		t.Fatalf("fourth call: status = %d, want 202", status)
	}
	if got := len(s.RequestsTo("POST", "/notify")); got != 4 {
		t.Errorf("recorded = %d, want 4 (failed calls are recorded too)", got)
	}

	// Other paths are unaffected, and a negative count fails until cleared.
	s.FailNext("POST", "/notify", http.StatusInternalServerError, -1)
	for i := 0; i < 5; i++ {
		if status, _ := do(t, "POST", url+"/notify", notifyBody("k9")); status != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", status)
		}
	}
	if status, _ := do(t, "POST", url+"/feedback", map[string]any{"signal": "acted"}); status != http.StatusAccepted {
		t.Fatalf("/feedback status = %d, want 202", status)
	}
	s.ClearFailures()
	if status, _ := do(t, "POST", url+"/notify", notifyBody("k9")); status != http.StatusAccepted {
		t.Fatalf("after clear: status = %d, want 202", status)
	}
}

func TestFeedbackAndRules(t *testing.T) {
	s, url := start(t)

	if status, _ := do(t, "POST", url+"/feedback", map[string]any{"signal": "shrugged"}); status != http.StatusBadRequest {
		t.Errorf("unknown signal: status = %d, want 400", status)
	}
	for _, sig := range []string{"acted", "dismissed", "snoozed", "muted"} {
		if status, _ := do(t, "POST", url+"/feedback", map[string]any{"signal": sig, "notificationId": "n1"}); status != http.StatusAccepted {
			t.Errorf("signal %s: status = %d", sig, status)
		}
	}
	if got := len(s.Feedback()); got != 4 {
		t.Errorf("feedback = %d, want 4", got)
	}

	do(t, "POST", url+"/rules", map[string]any{"payloadMatch": map[string]any{"priority": "p4_low"}, "channels": []string{"digest"}})
	if got := len(s.Rules()); got != 1 {
		t.Errorf("rules = %d, want 1", got)
	}
}

func TestLearningSuggestions(t *testing.T) {
	s, url := start(t)

	status, _ := do(t, "POST", url+"/learning/suggestions", map[string]any{"tenantId": "t1", "proposedAdjustment": 25})
	if status != http.StatusBadRequest {
		t.Fatalf("out-of-range adjustment: status = %d, want 400", status)
	}

	status, created := do(t, "POST", url+"/learning/suggestions", map[string]any{"tenantId": "t1", "proposedAdjustment": -10, "signalCount": 6})
	if status != http.StatusCreated || created["status"] != "PENDING_APPROVAL" {
		t.Fatalf("create: status %d body %v", status, created)
	}
	id := created["id"].(string)
	s.SeedSuggestion(map[string]any{"tenantId": "t2", "proposedAdjustment": 5.0})

	_, list := do(t, "GET", url+"/learning/suggestions?tenantId=t1", nil)
	if got := len(list["suggestions"].([]any)); got != 1 {
		t.Errorf("suggestions for t1 = %d, want 1", got)
	}

	if status, _ := do(t, "POST", url+"/learning/suggestions/"+id+"/decision", map[string]any{"decision": "maybe"}); status != http.StatusBadRequest {
		t.Errorf("bad decision: status = %d, want 400", status)
	}
	status, decided := do(t, "POST", url+"/learning/suggestions/"+id+"/decision", map[string]any{"decision": "approve"})
	if status != http.StatusOK || decided["status"] != "APPROVED" {
		t.Fatalf("approve: status %d body %v", status, decided)
	}
	if status, _ := do(t, "POST", url+"/learning/suggestions/"+id+"/decision", map[string]any{"decision": "reject"}); status != http.StatusConflict {
		t.Errorf("second decision: status = %d, want 409", status)
	}
	if status, _ := do(t, "POST", url+"/learning/suggestions/nope/decision", map[string]any{"decision": "approve"}); status != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", status)
	}
}

func TestInAppFeedAndReset(t *testing.T) {
	s, url := start(t)
	s.SeedFeed("t1", "u1", FeedItem{"id": "n1", "title": "hello"})

	_, mine := do(t, "GET", url+"/internal/v1/tenants/t1/users/u1/in-app-feed", nil)
	if got := len(mine["notifications"].([]any)); got != 1 {
		t.Errorf("feed for u1 = %d, want 1", got)
	}
	_, other := do(t, "GET", url+"/internal/v1/tenants/t1/users/u2/in-app-feed", nil)
	if got := len(other["notifications"].([]any)); got != 0 {
		t.Errorf("feed for u2 = %d, want 0", got)
	}

	s.Reset()
	if len(s.Requests()) != 0 || len(s.Notifications()) != 0 {
		t.Error("Reset left state behind")
	}
}
