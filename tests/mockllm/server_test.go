package mockllm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/tests/mockllm"
)

// TestScriptsThroughTheRealClient: Taskify's OpenAI-compatible client reads
// the mock's answers as a real server's.
func TestScriptsThroughTheRealClient(t *testing.T) {
	mock := mockllm.New()
	srv := httptest.NewServer(mock)
	defer srv.Close()
	client, err := llm.NewOpenAICompat(llm.Config{BaseURL: srv.URL + "/v1", Model: "mock", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(text string) llm.Response {
		t.Helper()
		resp, err := client.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: text}}})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	bad := `{"task_id": `
	mock.Add(mockllm.Script{Match: "task-1", Answers: []mockllm.Answer{
		{ToolCalls: []mockllm.ToolCall{{Name: "UpdateTaskAttributes", Arguments: json.RawMessage(`{"task_id":"t","labels":["a"]}`)}}},
		{ToolCalls: []mockllm.ToolCall{{Name: "UpdateTaskAttributes", RawArguments: &bad}}},
	}, Repeat: &mockllm.Answer{Content: "again"}})

	r := ask("About task-1")
	if len(r.Message.ToolCalls) != 1 || r.Message.ToolCalls[0].Name != "UpdateTaskAttributes" ||
		r.Message.ToolCalls[0].Arguments != `{"task_id":"t","labels":["a"]}` || r.FinishReason != "tool_calls" || r.Usage.PromptTokens == 0 {
		t.Errorf("first answer = %+v", r)
	}
	if r := ask("task-1 again"); r.Message.ToolCalls[0].Arguments != bad {
		t.Errorf("raw arguments = %q", r.Message.ToolCalls[0].Arguments)
	}
	if r := ask("task-1, third"); r.Message.Content != "again" || len(r.Message.ToolCalls) != 0 {
		t.Errorf("repeat = %+v", r)
	}
	if r := ask("something else"); r.Message.Content != mockllm.DefaultAnswer {
		t.Errorf("default = %+v", r)
	}
	if n := len(mock.RequestsMatching("task-1")); n != 3 || len(mock.Requests()) != 4 {
		t.Errorf("requests: %d matching, %d in all", n, len(mock.Requests()))
	}

	// The same over HTTP, as cmd/mock-llm serves it.
	body, _ := json.Marshal([]mockllm.Script{{Match: "task-2", Answers: []mockllm.Answer{{Content: "scripted"}}}})
	post := func(path string, body []byte) int {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if status := post("/_mock/scripts", body); status != 204 {
		t.Fatalf("add over HTTP: %d", status)
	}
	if r := ask("task-2"); r.Message.Content != "scripted" {
		t.Errorf("scripted over HTTP = %+v", r)
	}
	resp, err := http.Get(srv.URL + "/_mock/requests")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("requests over HTTP: %v", err)
	}
	var listed struct{ Requests []mockllm.Request }
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	_ = resp.Body.Close()
	if len(listed.Requests) != 5 {
		t.Errorf("listed %d requests", len(listed.Requests))
	}
	if status := post("/_mock/reset", nil); status != 204 || len(mock.Requests()) != 0 {
		t.Errorf("reset: %d, %d requests left", status, len(mock.Requests()))
	}
	for path, want := range map[string]int{"/_mock/scripts": 400, "/v1/chat/completions": 400, "/nope": 404} {
		if status := post(path, []byte("{")); status != want {
			t.Errorf("POST %s: %d, want %d", path, status, want)
		}
	}
}
