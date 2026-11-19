package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMockReplaysAndRecords(t *testing.T) {
	boom := errors.New("boom")
	m := NewMock(
		Call("c1", "UpdateTaskAttributes", map[string]any{"labels": []string{"bug"}}),
		Call("c2", "UpdateTaskAttributes", `{"labels": [`), // malformed, verbatim
		Fail(boom),
	).Then(Text("done"))

	conv := []Message{{Role: RoleUser, Content: "task"}}
	r1, err := m.Complete(ctx, Request{Messages: conv, Tools: []ToolSchema{{Name: "UpdateTaskAttributes"}}})
	if err != nil || r1.Message.ToolCalls[0] != (ToolCall{ID: "c1", Name: "UpdateTaskAttributes", Arguments: `{"labels":["bug"]}`}) || r1.FinishReason != "tool_calls" {
		t.Fatalf("step 1 = %+v, %v", r1, err)
	}
	// The caller appending to its conversation must not change the record.
	conv = append(conv, r1.Message)
	conv[0].Content = "changed"
	r1.Message.ToolCalls[0].Name = "mutated"

	r2, _ := m.Complete(ctx, Request{Messages: conv})
	if r2.Message.ToolCalls[0].Arguments != `{"labels": [` {
		t.Errorf("step 2 = %+v", r2)
	}
	if _, err := m.Complete(ctx, Request{}); !errors.Is(err, boom) {
		t.Errorf("step 3 = %v", err)
	}
	if r4, _ := m.Complete(ctx, Request{}); r4.Message.Content != "done" || r4.FinishReason != "stop" {
		t.Errorf("step 4 = %+v", r4)
	}
	if _, err := m.Complete(ctx, Request{}); !errors.Is(err, ErrScriptExhausted) {
		t.Errorf("past the end = %v", err)
	}

	reqs := m.Requests()
	if len(reqs) != 5 || m.Remaining() != 0 {
		t.Fatalf("%d requests, %d remaining", len(reqs), m.Remaining())
	}
	if reqs[0].Messages[0].Content != "task" || len(reqs[0].Messages) != 1 || reqs[0].Tools[0].Name != "UpdateTaskAttributes" {
		t.Errorf("request 1 recorded as %+v", reqs[0])
	}
	if len(reqs[1].Messages) != 2 || reqs[1].Messages[1].ToolCalls[0].Name != "mutated" {
		// The second request carried the caller's (mutated) copy, as sent.
		t.Errorf("request 2 recorded as %+v", reqs[1])
	}
	// The script's own step was not changed through the returned response.
	reqs[0].Messages[0].Content = "edited"
	if m.Requests()[0].Messages[0].Content != "task" {
		t.Error("Requests returned the recorded slice itself")
	}
}

func TestMockCancelledAndConcurrent(t *testing.T) {
	m := NewMock(Text("a"))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Complete(cctx, Request{}); !errors.Is(err, context.Canceled) || m.Remaining() != 1 || len(m.Requests()) != 0 {
		t.Errorf("cancelled call used a step: %v", err)
	}

	const n = 20
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Text("x")
	}
	m = NewMock(steps...)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Complete(ctx, Request{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m.Remaining() != 0 || len(m.Requests()) != n {
		t.Errorf("remaining %d, requests %d", m.Remaining(), len(m.Requests()))
	}
}
