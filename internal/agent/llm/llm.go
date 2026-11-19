// Package llm is the agent runtime's only way to reach a language model
// (docs/architecture.md section 5.3). The model never touches the database:
// it can only answer with text or with requests to call registered tools,
// which typed Go code then validates and runs.
//
// Client has two implementations: OpenAICompat, for any OpenAI-compatible
// chat completions endpoint (Ollama, vLLM, TGI, hosted models), and Mock,
// which replays a script for tests. Switching models is configuration only.
package llm

import (
	"context"
	"encoding/json"
)

// Client sends one chat completion request.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one entry in the conversation.
type Message struct {
	Role    string
	Content string
	// ToolCalls are the calls an assistant message asks for.
	ToolCalls []ToolCall
	// ToolCallID links a tool message to the call it answers.
	ToolCallID string
}

// ToolCall is a model's request to run a tool. Arguments is the model's raw
// text, which may not be valid JSON: the runtime validates it and spends its
// repair turn when it is not.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolSchema describes a tool offered to the model.
type ToolSchema struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object.
	Parameters json.RawMessage
}

// Request is one chat completion request.
type Request struct {
	Messages []Message
	Tools    []ToolSchema
	// Temperature is sent only when set.
	Temperature *float64
	// MaxTokens is sent only when positive.
	MaxTokens int
}

// Usage is the token count of one call.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Response is the model's answer.
type Response struct {
	// Message is the assistant message: text, tool calls, or both.
	Message Message
	// FinishReason is as reported, such as "stop" or "tool_calls".
	FinishReason string
	Usage        Usage
	// Model is the model that answered, as reported.
	Model string
}
