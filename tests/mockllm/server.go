// Package mockllm is a scriptable stand-in for an OpenAI-compatible model
// server, for the conformance suite (docs/architecture.md section 9.3).
//
// It answers POST {any prefix}/chat/completions. A script names a substring
// and the answers to give, in order, to requests whose messages contain it,
// so a test scripts the run of one job by its task's ID while other jobs get
// the default answer: a short final text with no tool calls, which changes
// nothing. Scripts and the received requests can be managed in process or
// over HTTP (the /_mock/ endpoints), so cmd/mock-llm serves the same mock.
//
// It must not import any Taskify server package: the conformance runner
// uses it as a black box.
package mockllm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// DefaultAnswer is the text given to a request no script matches.
const DefaultAnswer = "Nothing to change."

// ToolCall is a scripted tool call. Arguments is sent as the JSON-encoded
// string the OpenAI API uses; RawArguments, when set, is sent instead, as
// text, so a script can send malformed arguments.
type ToolCall struct {
	Name         string          `json:"name"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	RawArguments *string         `json:"raw_arguments,omitempty"`
}

// Answer is one scripted reply: text, tool calls, or both.
type Answer struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Script answers the requests whose messages contain Match, in order. Once
// its answers are used up, it gives Repeat's answer to further matches if
// set, and otherwise lets them fall through to the default.
type Script struct {
	Match   string   `json:"match"`
	Answers []Answer `json:"answers"`
	Repeat  *Answer  `json:"repeat,omitempty"`
}

type script struct {
	Script
	used int
}

// Request is one chat completion request the mock received.
type Request struct {
	Body json.RawMessage `json:"body"`
	// Script is the Match of the script that answered it, or "".
	Script string `json:"script"`
}

// Server is the mock. Use New.
type Server struct {
	mu       sync.Mutex
	scripts  []*script
	requests []Request
	calls    int
}

// New returns a mock with no scripts.
func New() *Server { return &Server{} }

// Add appends scripts. An earlier script wins when two match.
func (s *Server) Add(scripts ...Script) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sc := range scripts {
		s.scripts = append(s.scripts, &script{Script: sc})
	}
}

// Reset removes every script and forgets the requests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts, s.requests = nil, nil
}

// Requests returns the requests received, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// RequestsMatching returns the requests answered by the script with match.
func (s *Server) RequestsMatching(match string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Script == match {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
		s.complete(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/_mock/scripts":
		var in []Script
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "need a JSON list of scripts", http.StatusBadRequest)
			return
		}
		s.Add(in...)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/_mock/reset":
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/_mock/requests":
		writeJSON(w, http.StatusOK, map[string]any{"requests": s.Requests()})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil || !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "body is not JSON"}})
		return
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	var text strings.Builder
	for _, m := range req.Messages {
		text.WriteString(m.Content)
		text.WriteByte('\n')
	}

	s.mu.Lock()
	answer, matched := Answer{Content: DefaultAnswer}, ""
	for _, sc := range s.scripts {
		if sc.Match == "" || !strings.Contains(text.String(), sc.Match) {
			continue
		}
		switch {
		case sc.used < len(sc.Answers):
			answer, matched = sc.Answers[sc.used], sc.Match
			sc.used++
		case sc.Repeat != nil:
			answer, matched = *sc.Repeat, sc.Match
		default:
			continue
		}
		break
	}
	s.requests = append(s.requests, Request{Body: body, Script: matched})
	s.calls++
	n := s.calls
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, response(req.Model, answer, n))
}

func response(model string, a Answer, n int) map[string]any {
	msg := map[string]any{"role": "assistant", "content": a.Content}
	finish := "stop"
	if len(a.ToolCalls) > 0 {
		finish = "tool_calls"
		calls := make([]map[string]any, len(a.ToolCalls))
		for i, tc := range a.ToolCalls {
			args := string(tc.Arguments)
			if args == "" {
				args = "{}"
			}
			if tc.RawArguments != nil {
				args = *tc.RawArguments
			}
			calls[i] = map[string]any{"id": fmt.Sprintf("call_%d_%d", n, i), "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": args}}
		}
		msg["tool_calls"] = calls
	}
	return map[string]any{
		"id": fmt.Sprintf("chatcmpl-%d", n), "object": "chat.completion", "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 50, "completion_tokens": 10, "total_tokens": 60},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
