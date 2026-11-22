package mockagentpulse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
	defer func() { _ = resp.Body.Close() }()
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

	status, first := do(t, "POST", url+"/v1/notify", notifyBody("k1"))
	if status != http.StatusAccepted || first["duplicate"] != false {
		t.Fatalf("first call: status %d body %v", status, first)
	}
	status, second := do(t, "POST", url+"/v1/notify", notifyBody("k1"))
	if status != http.StatusOK || second["duplicate"] != true || second["id"] != first["id"] {
		t.Fatalf("second call: status %d body %v", status, second)
	}
	do(t, "POST", url+"/v1/notify", notifyBody("k2"))
	// Keys are unique per tenant: another tenant's k1 is a new notification.
	other := notifyBody("k1")
	other["tenantId"] = "tenant_other"
	if status, _ := do(t, "POST", url+"/v1/notify", other); status != http.StatusAccepted {
		t.Errorf("same key in another tenant: status %d", status)
	}

	if got := len(s.Notifications()); got != 3 {
		t.Errorf("accepted notifications = %d, want 3", got)
	}
	if got := s.Duplicates(); got != 1 {
		t.Errorf("duplicates = %d, want 1", got)
	}
	if got := len(s.RequestsTo("POST", "/v1/notify")); got != 4 {
		t.Errorf("recorded /v1/notify requests = %d, want 4", got)
	}
}

func TestNotifyDedupUnderConcurrency(t *testing.T) {
	s, url := start(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			do(t, "POST", url+"/v1/notify", notifyBody(fmt.Sprintf("k%d", i%10)))
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

// TestNotifyRefusesInvalidBodies covers AgentPulse's 40b rules: each invalid
// case is a 400 whose message names it, and nothing is accepted.
func TestNotifyRefusesInvalidBodies(t *testing.T) {
	s, url := start(t)
	with := func(change func(b map[string]any)) map[string]any {
		b := notifyBody("k")
		change(b)
		return b
	}
	payload := func(p map[string]any) map[string]any {
		return with(func(b map[string]any) { b["payload"] = p })
	}
	for name, tc := range map[string]struct {
		body map[string]any
		want string
	}{
		"missing userId":        {with(func(b map[string]any) { delete(b, "userId") }), "userId is required"},
		"missing tenant":        {with(func(b map[string]any) { delete(b, "tenantId") }), "tenantId is required"},
		"tenant with space":     {with(func(b map[string]any) { b["tenantId"] = "tenant acme" }), `tenantId "tenant acme" is invalid`},
		"tenant padded":         {with(func(b map[string]any) { b["tenantId"] = " tenant_acme" }), `tenantId " tenant_acme" is invalid`},
		"tenant leading dash":   {with(func(b map[string]any) { b["tenantId"] = "-acme" }), `tenantId "-acme" is invalid`},
		"tenant too long":       {with(func(b map[string]any) { b["tenantId"] = strings.Repeat("a", 65) }), "is invalid"},
		"tenant default":        {with(func(b map[string]any) { b["tenantId"] = "default" }), `tenantId "default" is reserved`},
		"key too long":          {with(func(b map[string]any) { b["idempotencyKey"] = strings.Repeat("k", 321) }), "idempotencyKey must be 1 to 320 printable ASCII"},
		"key not ASCII":         {with(func(b map[string]any) { b["idempotencyKey"] = "ké" }), "idempotencyKey must be 1 to 320 printable ASCII"},
		"payload not an object": {with(func(b map[string]any) { b["payload"] = "x" }), "payload must be an object"},
		"unknown action key":    {payload(map[string]any{"taskId": "t1", "actionUrls": map[string]any{"acknowledge": "https://x"}}), `payload.actionUrls key "acknowledge" is not supported`},
		"complete without task": {payload(map[string]any{"actionUrls": map[string]any{"complete": "https://x"}}), "payload.actionUrls.complete needs payload.taskId"},
		"snooze without task":   {payload(map[string]any{"actionUrls": map[string]any{"snooze_2h": "https://x"}}), "payload.actionUrls.snooze_2h needs payload.taskId"},
		"unblock without task":  {payload(map[string]any{"jobId": "j1", "actionUrls": map[string]any{"unblock": "https://x"}}), "payload.actionUrls.unblock needs payload.taskId"},
		"approve without job":   {payload(map[string]any{"taskId": "t1", "actionUrls": map[string]any{"approve": "https://x"}}), "payload.actionUrls.approve needs payload.jobId"},
		"reject without job":    {payload(map[string]any{"taskId": "t1", "actionUrls": map[string]any{"reject": "https://x"}}), "payload.actionUrls.reject needs payload.jobId"},
		"action URL not text":   {payload(map[string]any{"actionUrls": map[string]any{"view": 3}}), "payload.actionUrls.view must be a URL string"},
		"actionUrls not object": {payload(map[string]any{"actionUrls": []any{"view"}}), "payload.actionUrls must be an object"},
		"unknown label key":     {payload(map[string]any{"actionLabels": map[string]any{"acknowledge": "OK"}}), `payload.actionLabels key "acknowledge" is not supported`},
	} {
		status, out := do(t, "POST", url+"/v1/notify", tc.body)
		msg, _ := out["error"].(string)
		if status != http.StatusBadRequest || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: status %d error %q, want 400 naming %q", name, status, msg, tc.want)
		}
	}
	if len(s.Notifications()) != 0 {
		t.Error("an invalid notification was accepted")
	}
}

func TestNotifyAcceptsValidActions(t *testing.T) {
	s, url := start(t)
	for name, p := range map[string]map[string]any{
		"no actionUrls": {"taskId": "t1"},
		"only view":     {"actionUrls": map[string]any{"view": "https://x/brief"}},
		"task actions": {"taskId": "t1", "actionUrls": map[string]any{"complete": "https://x/c", "snooze_2h": "https://x/s", "unblock": "https://x/u", "view": "https://x"},
			"actionLabels": map[string]any{"unblock": "Unblock and set In Progress"}},
		"job actions": {"jobId": "j1", "actionUrls": map[string]any{"approve": "https://x/a", "reject": "https://x/r", "view": "https://x"}},
		"null urls":   {"actionUrls": nil},
	} {
		b := notifyBody("k-" + name)
		b["payload"] = p
		if status, out := do(t, "POST", url+"/v1/notify", b); status != http.StatusAccepted {
			t.Errorf("%s: status %d %v", name, status, out)
		}
	}
	// Tenant IDs are case-sensitive and may use dots and hyphens.
	for _, tenant := range []string{"Tenant.A-1_b", "Default", "a", strings.Repeat("z", 64)} {
		b := notifyBody("k-" + tenant)
		b["tenantId"] = tenant
		if status, out := do(t, "POST", url+"/v1/notify", b); status != http.StatusAccepted {
			t.Errorf("tenant %q: status %d %v", tenant, status, out)
		}
	}
	if got := len(s.Notifications()); got != 9 {
		t.Errorf("accepted = %d, want 9", got)
	}
}

func TestRequestsAreRecorded(t *testing.T) {
	s, url := start(t)
	do(t, "POST", url+"/v1/notify", notifyBody("k1"))
	if status, _ := do(t, "GET", url+"/healthz", nil); status != http.StatusOK {
		t.Errorf("/healthz status = %d", status)
	}

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
	// The pre-v1 paths are gone.
	for _, path := range []string{"/notify", "/feedback", "/rules"} {
		if status, _ := do(t, "POST", url+path, notifyBody("old")); status != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", path, status)
		}
	}
}

func TestScriptedFailures(t *testing.T) {
	s, url := start(t)
	s.FailNext("POST", "/v1/notify", http.StatusServiceUnavailable, 3)

	for i := 0; i < 3; i++ {
		if status, _ := do(t, "POST", url+"/v1/notify", notifyBody("k1")); status != http.StatusServiceUnavailable {
			t.Fatalf("call %d: status = %d, want 503", i+1, status)
		}
	}
	if len(s.Notifications()) != 0 {
		t.Fatal("a failed call was accepted")
	}
	if status, _ := do(t, "POST", url+"/v1/notify", notifyBody("k1")); status != http.StatusAccepted {
		t.Fatalf("fourth call: status = %d, want 202", status)
	}
	if got := len(s.RequestsTo("POST", "/v1/notify")); got != 4 {
		t.Errorf("recorded = %d, want 4 (failed calls are recorded too)", got)
	}

	// Other paths are unaffected, and a negative count fails until cleared.
	s.FailNext("POST", "/v1/notify", http.StatusInternalServerError, -1)
	for i := 0; i < 5; i++ {
		if status, _ := do(t, "POST", url+"/v1/notify", notifyBody("k9")); status != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", status)
		}
	}
	if status, _ := do(t, "POST", url+"/v1/feedback", feedbackBody("acted")); status != http.StatusAccepted {
		t.Fatalf("/v1/feedback status = %d, want 202", status)
	}
	s.ClearFailures()
	if status, _ := do(t, "POST", url+"/v1/notify", notifyBody("k9")); status != http.StatusAccepted {
		t.Fatalf("after clear: status = %d, want 202", status)
	}
}

func feedbackBody(signal string) map[string]any {
	return map[string]any{"notificationId": "n1", "tenantId": "tenant_acme", "userId": "usr_1", "signal": signal}
}

func TestFeedback(t *testing.T) {
	s, url := start(t)
	for _, sig := range []string{"acted", "dismissed", "snoozed", "muted"} {
		if status, _ := do(t, "POST", url+"/v1/feedback", feedbackBody(sig)); status != http.StatusAccepted {
			t.Errorf("signal %s: status = %d", sig, status)
		}
	}
	for name, tc := range map[string]struct {
		change func(b map[string]any)
		want   string
	}{
		"unknown signal": {func(b map[string]any) { b["signal"] = "shrugged" }, "signal must be"},
		"no tenant":      {func(b map[string]any) { delete(b, "tenantId") }, "tenantId"},
		"bad tenant":     {func(b map[string]any) { b["tenantId"] = "a/b" }, `tenantId "a/b" is invalid`},
		"no user":        {func(b map[string]any) { delete(b, "userId") }, "userId is required"},
	} {
		b := feedbackBody("acted")
		tc.change(b)
		status, out := do(t, "POST", url+"/v1/feedback", b)
		if msg, _ := out["error"].(string); status != http.StatusBadRequest || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: status %d error %q", name, status, msg)
		}
	}
	if got := len(s.Feedback()); got != 4 {
		t.Errorf("feedback = %d, want 4", got)
	}
}

func ruleBody(tenant string) map[string]any {
	return map[string]any{
		"tenantId": tenant, "name": "low",
		"matchAny": []any{map[string]any{"payloadMatch": map[string]any{"priority": "p4_low"}}},
		"channels": []string{"digest"},
	}
}

func TestRules(t *testing.T) {
	s, url := start(t)
	status, created := do(t, "POST", url+"/v1/rules", ruleBody("t1"))
	if status != http.StatusCreated || created["id"] == "" {
		t.Fatalf("create: %d %v", status, created)
	}
	critical := ruleBody("t1")
	critical["attentionClass"] = "critical"
	critical["matchAny"] = []any{map[string]any{"eventTypes": []any{"task.escalated"}}, map[string]any{"payloadMatch": map[string]any{"priority": "p1_critical"}}}
	if status, out := do(t, "POST", url+"/v1/rules", critical); status != http.StatusCreated {
		t.Fatalf("critical rule: %d %v", status, out)
	}
	do(t, "POST", url+"/v1/rules", ruleBody("t2"))
	if got := len(s.Rules()); got != 3 {
		t.Errorf("rules = %d, want 3", got)
	}

	_, list := do(t, "GET", url+"/v1/rules?tenantId=t1", nil)
	rules, _ := list["rules"].([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["id"] != created["id"] {
		t.Errorf("rules for t1 = %v", list)
	}
	if status, _ := do(t, "GET", url+"/v1/rules", nil); status != http.StatusBadRequest {
		t.Errorf("list without a tenant: status %d", status)
	}

	for name, tc := range map[string]struct {
		change func(b map[string]any)
		want   string
	}{
		"bad tenant":     {func(b map[string]any) { b["tenantId"] = "default" }, "reserved"},
		"no name":        {func(b map[string]any) { delete(b, "name") }, "name is required"},
		"no entries":     {func(b map[string]any) { b["matchAny"] = []any{} }, "matchAny needs at least one entry"},
		"unknown class":  {func(b map[string]any) { b["attentionClass"] = "urgent" }, "attentionClass must be"},
		"empty critical": {func(b map[string]any) { b["attentionClass"] = "critical"; b["matchAny"] = []any{map[string]any{}} }, "matchAny[0] of a critical rule needs"},
		"half critical": {func(b map[string]any) {
			b["attentionClass"] = "critical"
			b["matchAny"] = append(b["matchAny"].([]any), map[string]any{"eventTypes": []any{}})
		}, "matchAny[1] of a critical rule needs"},
		"entry not a map": {func(b map[string]any) { b["matchAny"] = []any{"x"} }, "matchAny[0] must be an object"},
	} {
		b := ruleBody("t1")
		tc.change(b)
		status, out := do(t, "POST", url+"/v1/rules", b)
		if msg, _ := out["error"].(string); status != http.StatusBadRequest || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: status %d error %q", name, status, msg)
		}
	}
	// A standard rule may have a catch-all entry.
	open := ruleBody("t1")
	open["attentionClass"] = "background"
	open["matchAny"] = []any{map[string]any{}}
	if status, out := do(t, "POST", url+"/v1/rules", open); status != http.StatusCreated {
		t.Errorf("background catch-all: %d %v", status, out)
	}
}

func TestLearningSuggestions(t *testing.T) {
	s, url := start(t)

	status, _ := do(t, "POST", url+"/v1/learning/suggestions", map[string]any{"tenantId": "t1", "proposedAdjustment": 25})
	if status != http.StatusBadRequest {
		t.Fatalf("out-of-range adjustment: status = %d, want 400", status)
	}
	if status, _ := do(t, "POST", url+"/v1/learning/suggestions", map[string]any{"tenantId": "t 1", "proposedAdjustment": 5}); status != http.StatusBadRequest {
		t.Errorf("bad tenant: status = %d, want 400", status)
	}

	status, created := do(t, "POST", url+"/v1/learning/suggestions", map[string]any{"tenantId": "t1", "proposedAdjustment": -10, "signalCount": 6})
	if status != http.StatusCreated || created["status"] != "PENDING_APPROVAL" {
		t.Fatalf("create: status %d body %v", status, created)
	}
	id := created["id"].(string)
	s.SeedSuggestion(map[string]any{"tenantId": "t2", "proposedAdjustment": 5.0})

	_, list := do(t, "GET", url+"/v1/learning/suggestions?tenantId=t1", nil)
	if got := len(list["suggestions"].([]any)); got != 1 {
		t.Errorf("suggestions for t1 = %d, want 1", got)
	}
	if status, _ := do(t, "GET", url+"/v1/learning/suggestions", nil); status != http.StatusBadRequest {
		t.Errorf("list without a tenant: status = %d, want 400", status)
	}

	if status, _ := do(t, "POST", url+"/v1/learning/suggestions/"+id+"/decision", map[string]any{"decision": "maybe"}); status != http.StatusBadRequest {
		t.Errorf("bad decision: status = %d, want 400", status)
	}
	status, decided := do(t, "POST", url+"/v1/learning/suggestions/"+id+"/decision", map[string]any{"decision": "approve"})
	if status != http.StatusOK || decided["status"] != "APPROVED" {
		t.Fatalf("approve: status %d body %v", status, decided)
	}
	if status, _ := do(t, "POST", url+"/v1/learning/suggestions/"+id+"/decision", map[string]any{"decision": "reject"}); status != http.StatusConflict {
		t.Errorf("second decision: status = %d, want 409", status)
	}
	if status, _ := do(t, "POST", url+"/v1/learning/suggestions/nope/decision", map[string]any{"decision": "approve"}); status != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", status)
	}
}

func TestInAppFeedAndReset(t *testing.T) {
	s, url := start(t)
	s.SeedFeed("t1", "u1", FeedItem{"id": "n3"}, FeedItem{"id": "n2"}, FeedItem{"id": "n1"})

	_, page := do(t, "GET", url+"/v1/tenants/t1/users/u1/in-app-feed?limit=2", nil)
	if got := page["notifications"].([]any); len(got) != 2 || got[0].(map[string]any)["id"] != "n3" || page["nextCursor"] != "2" {
		t.Errorf("first page = %v", page)
	}
	_, rest := do(t, "GET", url+"/v1/tenants/t1/users/u1/in-app-feed?limit=2&cursor=2", nil)
	if got := rest["notifications"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != "n1" || rest["nextCursor"] != nil {
		t.Errorf("second page = %v", rest)
	}
	_, other := do(t, "GET", url+"/v1/tenants/t1/users/u2/in-app-feed", nil)
	if got := len(other["notifications"].([]any)); got != 0 {
		t.Errorf("feed for u2 = %d, want 0", got)
	}
	for _, path := range []string{"/v1/tenants/default/users/u1/in-app-feed", "/v1/tenants/t1/users/u1/in-app-feed?limit=0", "/v1/tenants/t1/users/u1/in-app-feed?cursor=x"} {
		if status, _ := do(t, "GET", url+path, nil); status != http.StatusBadRequest {
			t.Errorf("GET %s: status %d, want 400", path, status)
		}
	}
	if status, _ := do(t, "GET", url+"/internal/v1/tenants/t1/users/u1/in-app-feed", nil); status != http.StatusNotFound {
		t.Errorf("the pre-v1 feed path: status %d, want 404", status)
	}

	s.Reset()
	if len(s.Requests()) != 0 || len(s.Notifications()) != 0 {
		t.Error("Reset left state behind")
	}
}

func TestSetDelay(t *testing.T) {
	s, url := start(t)
	s.SetDelay("POST", "/v1/notify", 150*time.Millisecond)
	began := time.Now()
	if status, _ := do(t, "POST", url+"/v1/notify", notifyBody("slow")); status != http.StatusAccepted {
		t.Fatalf("status = %d", status)
	}
	if elapsed := time.Since(began); elapsed < 150*time.Millisecond {
		t.Errorf("answered after %v, want at least 150ms", elapsed)
	}
	s.SetDelay("POST", "/v1/notify", 0)
	began = time.Now()
	do(t, "POST", url+"/v1/notify", notifyBody("fast"))
	if time.Since(began) > 100*time.Millisecond {
		t.Error("the delay was not removed")
	}
}
