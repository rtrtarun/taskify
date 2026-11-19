package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

// stub is a chat completions server answering with a list of handlers in
// turn, recording each request body.
type stub struct {
	t        *testing.T
	mu       sync.Mutex
	handlers []http.HandlerFunc
	bodies   []map[string]any
	headers  []http.Header
	calls    atomic.Int32
}

func newStub(t *testing.T, handlers ...http.HandlerFunc) (*stub, *httptest.Server) {
	s := &stub{t: t, handlers: handlers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		n := int(s.calls.Add(1)) - 1
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		if n >= len(s.handlers) {
			t.Errorf("unexpected call %d", n+1)
			w.WriteHeader(500)
			return
		}
		s.handlers[n](w, r)
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func status(code int, body string, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

const toolCallBody = `{
  "model": "qwen2.5-coder:7b",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": null,
      "tool_calls": [{
        "id": "call_1",
        "type": "function",
        "function": {"name": "UpdateTaskAttributes", "arguments": "{\"task_id\":\"t1\",\"labels\":[\"bug\"]}"}
      }]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens": 120, "completion_tokens": 18, "total_tokens": 138}
}`

func ok(body string) http.HandlerFunc { return status(200, body, "Content-Type", "application/json") }

func newClient(t *testing.T, url string, cfg Config) *OpenAICompat {
	t.Helper()
	cfg.BaseURL, cfg.Model = url+"/v1/", "qwen2.5-coder:7b"
	if cfg.Backoff == nil {
		cfg.Backoff = func(int) time.Duration { return time.Millisecond }
	}
	c, err := NewOpenAICompat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestToolCallResponse(t *testing.T) {
	s, srv := newStub(t, ok(toolCallBody))
	c := newClient(t, srv.URL, Config{APIKey: "sk-test"})
	temp := 0.2
	resp, err := c.Complete(ctx, Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "You triage tasks."},
			{Role: RoleUser, Content: "Task: checkout crashes"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "GetTask", Arguments: `{"task_id":"t1"}`}}},
			{Role: RoleTool, ToolCallID: "call_0", Content: `{"title":"checkout crashes"}`},
		},
		Tools: []ToolSchema{
			{Name: "UpdateTaskAttributes", Description: "Set labels", Parameters: json.RawMessage(`{"type":"object","properties":{"labels":{"type":"array"}}}`)},
			{Name: "NoArgs"},
		},
		Temperature: &temp,
		MaxTokens:   512,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := ToolCall{ID: "call_1", Name: "UpdateTaskAttributes", Arguments: `{"task_id":"t1","labels":["bug"]}`}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0] != want {
		t.Errorf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if resp.Message.Role != RoleAssistant || resp.Message.Content != "" || resp.FinishReason != "tool_calls" ||
		resp.Usage != (Usage{PromptTokens: 120, CompletionTokens: 18}) || resp.Model != "qwen2.5-coder:7b" {
		t.Errorf("response = %+v", resp)
	}

	// What was sent.
	if got := s.headers[0].Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	body := s.bodies[0]
	if body["model"] != "qwen2.5-coder:7b" || body["temperature"] != 0.2 || body["max_tokens"] != 512.0 {
		t.Errorf("body = %v", body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %v", msgs)
	}
	asst := msgs[2].(map[string]any)
	call := asst["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	// OpenAI sends arguments as a JSON-encoded string.
	if call["type"] != "function" || call["id"] != "call_0" || fn["name"] != "GetTask" || fn["arguments"] != `{"task_id":"t1"}` {
		t.Errorf("assistant tool call = %v", call)
	}
	if tool := msgs[3].(map[string]any); tool["role"] != "tool" || tool["tool_call_id"] != "call_0" {
		t.Errorf("tool message = %v", tool)
	}
	tools := body["tools"].([]any)
	first := tools[0].(map[string]any)["function"].(map[string]any)
	if first["name"] != "UpdateTaskAttributes" || first["parameters"].(map[string]any)["type"] != "object" {
		t.Errorf("tool schema = %v", first)
	}
	// A tool without parameters still sends a valid empty object schema.
	if p := tools[1].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any); p["type"] != "object" {
		t.Errorf("empty parameters = %v", p)
	}
}

func TestTextResponseAndOptionalFields(t *testing.T) {
	s, srv := newStub(t, ok(`{"model":"m","choices":[{"message":{"role":"assistant","content":"All done."},"finish_reason":"stop"}]}`))
	c := newClient(t, srv.URL, Config{}) // no API key, as for local Ollama
	resp, err := c.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil || resp.Message.Content != "All done." || len(resp.Message.ToolCalls) != 0 || resp.FinishReason != "stop" {
		t.Fatalf("Complete = %+v, %v", resp, err)
	}
	if got := s.headers[0].Get("Authorization"); got != "" {
		t.Errorf("Authorization sent without a key: %q", got)
	}
	for _, k := range []string{"tools", "temperature", "max_tokens"} {
		if _, ok := s.bodies[0][k]; ok {
			t.Errorf("%s sent although unset", k)
		}
	}
}

func TestArgumentsAsObject(t *testing.T) {
	// Some servers send arguments as an object rather than a string.
	_, srv := newStub(t, ok(`{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"F","arguments":{"a":1}}}]}}]}`))
	resp, err := newClient(t, srv.URL, Config{}).Complete(ctx, Request{})
	if err != nil || resp.Message.ToolCalls[0].Arguments != `{"a":1}` || resp.Message.Role != RoleAssistant {
		t.Errorf("Complete = %+v, %v", resp, err)
	}
	// Malformed arguments are passed through for the runtime to repair.
	_, srv = newStub(t, ok(`{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"F","arguments":"{\"a\":"}}]}}]}`))
	resp, err = newClient(t, srv.URL, Config{}).Complete(ctx, Request{})
	if err != nil || resp.Message.ToolCalls[0].Arguments != `{"a":` {
		t.Errorf("malformed = %+v, %v", resp, err)
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	s, srv := newStub(t, slow)
	c := newClient(t, srv.URL, Config{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Complete(ctx, Request{})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timed out after %s", d)
	}
	// A timeout is not retried.
	if n := s.calls.Load(); n != 1 {
		t.Errorf("%d calls, want 1", n)
	}

	// The caller's own deadline is reported as theirs, not as ErrTimeout.
	_, srv = newStub(t, slow)
	c = newClient(t, srv.URL, Config{Timeout: time.Minute})
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(short, Request{}); errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("caller deadline: error = %v", err)
	}
}

func TestRetryOn429And503(t *testing.T) {
	s, srv := newStub(t,
		status(429, `{"error":"rate limited"}`, "Retry-After", "0"),
		status(503, `{"error":"loading model"}`),
		ok(toolCallBody),
	)
	var waits []int
	c := newClient(t, srv.URL, Config{Backoff: func(n int) time.Duration { waits = append(waits, n); return time.Millisecond }})
	resp, err := c.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("Complete = %+v, %v", resp, err)
	}
	if n := s.calls.Load(); n != 3 {
		t.Errorf("%d calls, want 3", n)
	}
	if len(waits) != 2 || waits[0] != 1 || waits[1] != 2 {
		t.Errorf("backoff calls = %v, want [1 2]", waits)
	}
	// Every attempt sends the same body.
	if s.bodies[0]["model"] != s.bodies[2]["model"] || len(s.bodies[2]["messages"].([]any)) != 1 {
		t.Errorf("retried body differs: %v", s.bodies[2])
	}
}

func TestRetryLimitAndPermanentErrors(t *testing.T) {
	s, srv := newStub(t, status(503, "busy"), status(503, "busy"), status(503, "still busy"))
	c := newClient(t, srv.URL, Config{MaxRetries: 2, APIKey: "sk-secret"})
	_, err := c.Complete(ctx, Request{})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 || se.Body != "still busy" || s.calls.Load() != 3 {
		t.Errorf("after the retry limit: %v, %d calls", err, s.calls.Load())
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Error("the API key appears in an error")
	}

	for _, code := range []int{400, 401, 404, 500, 502} {
		s, srv := newStub(t, status(code, `{"error":"no"}`))
		_, err := newClient(t, srv.URL, Config{}).Complete(ctx, Request{})
		if !errors.As(err, &se) || se.Code != code || s.calls.Load() != 1 {
			t.Errorf("%d: %v after %d calls, want one call", code, err, s.calls.Load())
		}
	}

	s, srv = newStub(t, status(429, ""), status(429, ""))
	_, err = newClient(t, srv.URL, Config{MaxRetries: -1}).Complete(ctx, Request{})
	if !errors.As(err, &se) || s.calls.Load() != 1 {
		t.Errorf("MaxRetries -1: %v after %d calls", err, s.calls.Load())
	}
}

func TestCancelWhileWaitingToRetry(t *testing.T) {
	_, srv := newStub(t, status(429, ""))
	c := newClient(t, srv.URL, Config{Backoff: func(int) time.Duration { return time.Hour }})
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if _, err := c.Complete(cctx, Request{}); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("cancellation did not interrupt the wait")
	}
}

func TestBadResponses(t *testing.T) {
	for name, body := range map[string]string{"no choices": `{"choices":[]}`, "not JSON": `<html>`} {
		_, srv := newStub(t, ok(body))
		if _, err := newClient(t, srv.URL, Config{}).Complete(ctx, Request{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	_, srv := newStub(t, ok(`{"choices":[]}`))
	if _, err := newClient(t, srv.URL, Config{}).Complete(ctx, Request{}); !errors.Is(err, ErrNoChoice) {
		t.Errorf("no choices: %v", err)
	}
	// Nothing listening.
	c := newClient(t, "http://127.0.0.1:1", Config{})
	if _, err := c.Complete(ctx, Request{}); err == nil || errors.Is(err, ErrTimeout) {
		t.Errorf("connection refused: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := NewOpenAICompat(Config{Model: "m"}); err == nil {
		t.Error("missing base URL accepted")
	}
	if _, err := NewOpenAICompat(Config{BaseURL: "http://x"}); err == nil {
		t.Error("missing model accepted")
	}
	c, err := NewOpenAICompat(Config{BaseURL: "http://x/v1/", Model: "m"})
	if err != nil || c.timeout != DefaultTimeout || c.maxRetries != DefaultMaxRetries || c.url != "http://x/v1/chat/completions" {
		t.Errorf("defaults = %+v, %v", c, err)
	}
}

func TestExponentialBackoff(t *testing.T) {
	for n, ceiling := range map[int]time.Duration{1: 500 * time.Millisecond, 2: time.Second, 3: 2 * time.Second, 5: 8 * time.Second, 20: 8 * time.Second} {
		for range 50 {
			if d := ExponentialBackoff(n); d <= 0 || d > ceiling {
				t.Fatalf("ExponentialBackoff(%d) = %s, want (0, %s]", n, d, ceiling)
			}
		}
	}
	if retryAfter("2") != 2*time.Second || retryAfter("") != 0 || retryAfter("Wed, 21 Oct 2026 07:28:00 GMT") != 0 || retryAfter("-1") != 0 {
		t.Error("retryAfter")
	}
}
