package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/rtrtarun/taskify/internal/observability"
)

const (
	// DefaultTimeout bounds each attempt (LLM_TIMEOUT).
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRetries is how many times a 429 or 503 is retried.
	DefaultMaxRetries = 3
	// maxDelay caps one wait, including a server's Retry-After.
	maxDelay = 30 * time.Second
	// maxBody bounds a response body read into memory.
	maxBody = 8 << 20
)

// ErrTimeout is returned when one attempt exceeds the configured timeout. It
// is not retried: the runtime decides whether the job tries again.
var ErrTimeout = errors.New("llm: request timed out")

// errAttemptDeadline marks the expiry of our own per-attempt timeout, as
// opposed to the caller's deadline.
var errAttemptDeadline = errors.New("llm: attempt deadline")

// ErrNoChoice is returned for a 2xx response that carries no answer.
var ErrNoChoice = errors.New("llm: response has no choices")

// StatusError is a non-2xx response. Body is a short prefix of the response
// body; the request, which holds the API key in its headers, is never part of
// the error.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("llm: chat completion returned %d", e.Code)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// retryable reports the statuses that are retried with backoff.
func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// Config configures an OpenAICompat client.
type Config struct {
	// BaseURL is the API root, such as http://localhost:11434/v1 for Ollama.
	// The client posts to BaseURL + "/chat/completions".
	BaseURL string
	// APIKey is sent as a bearer token. Empty sends no Authorization header,
	// as local Ollama expects.
	APIKey string
	// Model is the model name sent with every request.
	Model string
	// Timeout bounds each attempt. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxRetries is how many times a 429 or 503 is retried. Zero means
	// DefaultMaxRetries; negative means never.
	MaxRetries int
	// Backoff returns the wait before retry number n (1-based). Nil means
	// ExponentialBackoff. A larger Retry-After from the server wins.
	Backoff func(n int) time.Duration
	// HTTPClient replaces the default traced client. Its own Timeout is
	// ignored in favour of Timeout.
	HTTPClient *http.Client
}

// ExponentialBackoff is the default wait: a random duration up to
// 500ms * 2^(n-1), capped at 8s (full jitter).
func ExponentialBackoff(n int) time.Duration {
	ceiling := min(500*time.Millisecond<<min(n-1, 4), 8*time.Second)
	return rand.N(ceiling) + 1
}

// OpenAICompat calls an OpenAI-compatible chat completions endpoint with
// tool calling.
type OpenAICompat struct {
	url        string
	apiKey     string
	model      string
	timeout    time.Duration
	maxRetries int
	backoff    func(int) time.Duration
	http       *http.Client
}

var _ Client = (*OpenAICompat)(nil)

// NewOpenAICompat returns a client, or an error if BaseURL or Model is
// missing.
func NewOpenAICompat(cfg Config) (*OpenAICompat, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: base URL is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("llm: model is required")
	}
	c := &OpenAICompat{
		url:        strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		timeout:    cfg.Timeout,
		maxRetries: cfg.MaxRetries,
		backoff:    cfg.Backoff,
		http:       cfg.HTTPClient,
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	switch {
	case c.maxRetries == 0:
		c.maxRetries = DefaultMaxRetries
	case c.maxRetries < 0:
		c.maxRetries = 0
	}
	if c.backoff == nil {
		c.backoff = ExponentialBackoff
	}
	if c.http == nil {
		// The OpenTelemetry transport records each call as a client span in
		// the job's trace.
		c.http = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport.(*http.Transport).Clone(),
			otelhttp.WithPropagators(observability.Propagator))}
	}
	return c, nil
}

// Complete sends the request, retrying 429 and 503 with backoff.
func (c *OpenAICompat) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(c.wireRequest(req))
	if err != nil {
		return Response{}, fmt.Errorf("llm: encode request: %w", err)
	}
	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.attempt(ctx, body)
		var se *StatusError
		if err == nil || !errors.As(err, &se) || !retryable(se.Code) || attempt >= c.maxRetries {
			return resp, err
		}
		wait := min(max(c.backoff(attempt+1), retryAfter), maxDelay)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return Response{}, fmt.Errorf("llm: waiting to retry: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// attempt makes one HTTP call within the per-attempt timeout.
func (c *OpenAICompat) attempt(ctx context.Context, body []byte) (Response, time.Duration, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, c.timeout, errAttemptDeadline)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Response{}, 0, fmt.Errorf("llm: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, 0, c.transportErr(ctx, err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBody))
	if err != nil {
		return Response{}, 0, c.transportErr(ctx, err)
	}
	if httpResp.StatusCode/100 != 2 {
		return Response{}, retryAfter(httpResp.Header.Get("Retry-After")),
			&StatusError{Code: httpResp.StatusCode, Body: prefix(raw)}
	}
	resp, err := parseResponse(raw)
	return resp, 0, err
}

// transportErr reports our own deadline as ErrTimeout, keeping a caller's
// cancellation as it is.
func (c *OpenAICompat) transportErr(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errAttemptDeadline) {
		return fmt.Errorf("%w after %s", ErrTimeout, c.timeout)
	}
	return fmt.Errorf("llm: %w", err)
}

// retryAfter reads a Retry-After header given in seconds. HTTP dates are
// rare from model servers and are ignored.
func retryAfter(h string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func prefix(b []byte) string {
	const n = 300
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "..."
	}
	return s
}

// Wire format of the OpenAI chat completions API.

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded string in the OpenAI API. Some servers
	// send a JSON object instead; parseArguments accepts both.
	Arguments json.RawMessage `json:"arguments"`
}

type wireTool struct {
	Type     string         `json:"type"`
	Function wireToolSchema `json:"function"`
}

type wireToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string         `json:"role"`
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (c *OpenAICompat) wireRequest(req Request) wireRequest {
	w := wireRequest{Model: c.model, Temperature: req.Temperature, MaxTokens: req.MaxTokens,
		Messages: make([]wireMessage, 0, len(req.Messages))}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Arguments) // a string always encodes
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: tc.ID, Type: "function",
				Function: wireFunction{Name: tc.Name, Arguments: args}})
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		w.Tools = append(w.Tools, wireTool{Type: "function",
			Function: wireToolSchema{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	return w
}

func parseResponse(raw []byte) (Response, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(w.Choices) == 0 {
		return Response{}, ErrNoChoice
	}
	ch := w.Choices[0]
	msg := Message{Role: ch.Message.Role}
	if msg.Role == "" {
		msg.Role = RoleAssistant
	}
	if ch.Message.Content != nil {
		msg.Content = *ch.Message.Content
	}
	for _, tc := range ch.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name,
			Arguments: parseArguments(tc.Function.Arguments)})
	}
	return Response{
		Message:      msg,
		FinishReason: ch.FinishReason,
		Usage:        Usage{PromptTokens: w.Usage.PromptTokens, CompletionTokens: w.Usage.CompletionTokens},
		Model:        w.Model,
	}, nil
}

// parseArguments returns the arguments text: the decoded string when the
// server sent a JSON string, as the OpenAI API does, or the raw JSON when it
// sent an object.
func parseArguments(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	return string(raw)
}
