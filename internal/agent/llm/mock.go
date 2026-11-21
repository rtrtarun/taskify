package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrScriptExhausted is returned by Mock when it has no step left.
var ErrScriptExhausted = errors.New("llm: mock script exhausted")

// Step is one scripted answer: a response, or an error when Err is set.
type Step struct {
	Response Response
	Err      error
}

// Mock replays a script of answers in order and records every request it
// was sent. It is safe for concurrent use; concurrent callers take steps in
// arrival order.
type Mock struct {
	mu       sync.Mutex
	script   []Step
	requests []Request
}

var _ Client = (*Mock)(nil)

// NewMock returns a mock that answers with steps in order.
func NewMock(steps ...Step) *Mock { return &Mock{script: steps} }

// Reply is a step answering with r.
func Reply(r Response) Step { return Step{Response: r} }

// Fail is a step answering with err.
func Fail(err error) Step { return Step{Err: err} }

// Text is a step answering with a final text message.
func Text(content string) Step {
	return Reply(Response{Message: Message{Role: RoleAssistant, Content: content}, FinishReason: "stop", Model: "mock"})
}

// Call is a step asking for one tool call. args is marshalled to JSON; a
// string is sent as is, so a test can script malformed arguments.
func Call(id, name string, args any) Step {
	text, ok := args.(string)
	if !ok {
		b, err := json.Marshal(args)
		if err != nil {
			panic(fmt.Sprintf("llm.Call: %v", err))
		}
		text = string(b)
	}
	return Reply(Response{
		Message:      Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: text}}},
		FinishReason: "tool_calls",
		Model:        "mock",
	})
}

// Then appends steps to the script.
func (m *Mock) Then(steps ...Step) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.script = append(m.script, steps...)
	return m
}

// Complete records req and returns the next step. A cancelled context fails
// without using a step, as a real call would.
func (m *Mock) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, cloneRequest(req))
	if len(m.script) == 0 {
		return Response{}, ErrScriptExhausted
	}
	step := m.script[0]
	m.script = m.script[1:]
	if step.Err != nil {
		return Response{}, step.Err
	}
	return cloneResponse(step.Response), nil
}

// Requests returns copies of the requests received so far, in order.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, len(m.requests))
	for i, r := range m.requests {
		out[i] = cloneRequest(r)
	}
	return out
}

// Remaining reports how many steps have not been used.
func (m *Mock) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.script)
}

// cloneRequest copies req so later changes by the caller, such as appending
// to the conversation, do not alter what was recorded.
func cloneRequest(req Request) Request {
	c := req
	c.Messages = make([]Message, len(req.Messages))
	for i, msg := range req.Messages {
		c.Messages[i] = cloneMessage(msg)
	}
	c.Tools = make([]ToolSchema, len(req.Tools))
	for i, t := range req.Tools {
		t.Parameters = slices.Clone(t.Parameters)
		c.Tools[i] = t
	}
	if req.Temperature != nil {
		v := *req.Temperature
		c.Temperature = &v
	}
	return c
}

func cloneResponse(r Response) Response {
	r.Message = cloneMessage(r.Message)
	return r
}

func cloneMessage(m Message) Message {
	m.ToolCalls = slices.Clone(m.ToolCalls)
	return m
}
