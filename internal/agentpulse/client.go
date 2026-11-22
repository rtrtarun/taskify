// Package agentpulse is Taskify's client for the AgentPulse
// notification-service.
//
// It calls the /v1/ API that AgentPulse builds in its milestone 40, as agreed
// in docs/blockers.md and described in docs/architecture.md section 5.5.
// Task M0-07 runs it against a real AgentPulse; until then every path and
// field name lives in this package, so a correction touches nothing else.
//
// The client never retries: the outbox relay owns retry, and the synchronous
// callers (feedback, admin proxies) surface the error to their own caller.
package agentpulse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/rtrtarun/taskify/internal/observability"
)

// AgentPulse paths (docs/blockers.md, "Agreed with AgentPulse").
const (
	pathNotify      = "/v1/notify"
	pathFeedback    = "/v1/feedback"
	pathRules       = "/v1/rules"
	pathSuggestions = "/v1/learning/suggestions"
	pathAnalytics   = "/v1/analytics/feedback"
	pathTenants     = "/v1/tenants/"
)

// ErrCircuitOpen is returned without contacting AgentPulse while the circuit
// breaker is open.
var ErrCircuitOpen = errors.New("agentpulse: circuit breaker open")

// StatusError is a non-2xx response.
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string // a short prefix of the response body
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("agentpulse: %s %s returned %d", e.Method, e.Path, e.Code)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Retryable reports whether a failed call may succeed later: transport
// errors, timeouts, 429, and 5xx. Other 4xx responses are permanent, and so
// is a nil error. ErrCircuitOpen is retryable.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	return true
}

// IsUpstream reports whether err is a failure to get a good answer from
// AgentPulse: an error response, an open circuit, or a network failure or
// timeout. Callers report these as 502 or 503 rather than as their own fault.
func IsUpstream(err error) bool {
	var se *StatusError
	var ue *url.Error
	return errors.As(err, &se) || errors.Is(err, ErrCircuitOpen) || errors.As(err, &ue)
}

// Client calls AgentPulse.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	breaker *Breaker
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.http = hc } }

// WithBreaker replaces the circuit breaker. nil disables it.
func WithBreaker(b *Breaker) Option { return func(c *Client) { c.breaker = b } }

// New returns a client with the transport settings from
// docs/architecture.md section 6.2 and the default circuit breaker.
func New(baseURL, apiKey string, opts ...Option) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 64
	transport.IdleConnTimeout = 90 * time.Second
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// The OpenTelemetry transport opens a client span for each call and
		// sends its W3C traceparent, continuing the trace in ctx.
		http: &http.Client{
			Transport: otelhttp.NewTransport(transport, otelhttp.WithPropagators(observability.Propagator)),
			Timeout:   10 * time.Second,
		},
		breaker: DefaultBreaker(),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// BreakerState reports the circuit breaker state, or closed if there is none.
func (c *Client) BreakerState() BreakerState {
	if c.breaker == nil {
		return BreakerClosed
	}
	return c.breaker.State()
}

// Notify sends a complete /v1/notify body, as stored in the outbox.
func (c *Client) Notify(ctx context.Context, body json.RawMessage) error {
	body, err := normalizeNotify(body)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, pathNotify, body, nil)
}

// normalizeNotify rewrites the legacy payload key bypass_digest to
// bypassDigest. Bodies without it are sent byte for byte.
func normalizeNotify(body json.RawMessage) (json.RawMessage, error) {
	if !bytes.Contains(body, []byte(`"bypass_digest"`)) {
		return body, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("agentpulse: notify body: %w", err)
	}
	if payload, ok := doc["payload"].(map[string]any); ok {
		if v, legacy := payload["bypass_digest"]; legacy {
			if _, current := payload["bypassDigest"]; !current {
				payload["bypassDigest"] = v
			}
			delete(payload, "bypass_digest")
		}
	}
	return json.Marshal(doc)
}

// Feedback is a user's reaction to a notification. It names the
// notification, the task, or both (AP-12). Without a NotificationID the
// field is left out, never sent empty, and AgentPulse counts the signal
// under the user's latest notification about TaskID.
type Feedback struct {
	NotificationID string `json:"notificationId,omitempty"`
	TenantID       string `json:"tenantId"`
	UserID         string `json:"userId"`
	Signal         string `json:"signal"` // acted, dismissed, snoozed, muted
	SnoozeDuration string `json:"snoozeDuration,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
}

// RecordFeedback forwards a feedback signal.
func (c *Client) RecordFeedback(ctx context.Context, fb Feedback) error {
	return c.do(ctx, http.MethodPost, pathFeedback, fb, nil)
}

// Rule is an AgentPulse routing rule. It applies when any entry of MatchAny
// matches: an entry matches when the event type is one of EventTypes (if
// set) and every PayloadMatch key equals the payload's value. MatchAny is how
// Taskify expresses the "A or B" conditions of its default rules; AgentPulse
// supports it natively.
//
// AttentionClass, when set, is the class a matching notification gets.
// AgentPulse runs its own locked critical rules first, then the tenant's
// rules, then defaults to standard. A critical rule needs a real condition,
// event types or payload values, in every entry. BypassDigest keeps an item
// out of the digest but never makes it critical: only AttentionCritical lets
// it through quiet hours.
type Rule struct {
	TenantID       string      `json:"tenantId"`
	Name           string      `json:"name"`
	AttentionClass string      `json:"attentionClass,omitempty"`
	MatchAny       []RuleMatch `json:"matchAny"`
	Channels       []string    `json:"channels"`
	BypassDigest   bool        `json:"bypassDigest,omitempty"`
}

// Attention classes a Rule may set.
const (
	AttentionCritical   = "critical"
	AttentionStandard   = "standard"
	AttentionBackground = "background"
)

// RuleMatch is one condition of a Rule.
type RuleMatch struct {
	EventTypes   []string       `json:"eventTypes,omitempty"`
	PayloadMatch map[string]any `json:"payloadMatch,omitempty"`
}

// CreateRule registers a rule and returns its ID.
func (c *Client) CreateRule(ctx context.Context, r Rule) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, pathRules, r, &out)
	return out.ID, err
}

// ListRules returns the tenant's rules as AgentPulse sends them:
// {"rules": [...]}.
func (c *Client) ListRules(ctx context.Context, tenantID string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, pathRules+"?tenantId="+url.QueryEscape(tenantID), nil, &out)
	return out, err
}

// ListLearningSuggestions returns AgentPulse's response unchanged, for the
// admin proxy to relay.
func (c *Client) ListLearningSuggestions(ctx context.Context, tenantID string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, pathSuggestions+"?tenantId="+url.QueryEscape(tenantID), nil, &out)
	return out, err
}

// ReviewLearningSuggestion approves or rejects one of the tenant's
// suggestions and returns AgentPulse's response unchanged. The body names
// the tenant: AgentPulse answers 404 for another tenant's suggestion, as
// for an unknown one (AP-12).
func (c *Client) ReviewLearningSuggestion(ctx context.Context, tenantID, id, decision, decidedBy string) (json.RawMessage, error) {
	var out json.RawMessage
	body := map[string]string{"tenantId": tenantID, "decision": decision, "decidedBy": decidedBy}
	err := c.do(ctx, http.MethodPost, pathSuggestions+"/"+url.PathEscape(id)+"/decision", body, &out)
	return out, err
}

// EventPattern names the scope a learning suggestion adjusts: one event type
// and one payload label. A nil Label is the notifications of that event type
// that carry no label; it is sent as null, never left out.
type EventPattern struct {
	EventType string  `json:"eventType"`
	Label     *string `json:"label"`
}

// LearningSuggestion is a suggestion an app submits (R6): a change to
// AgentPulse's learned score adjustment for a rule or an event pattern,
// within -20 to 20, resting on at least 5 signals from at least 2 users.
// AgentPulse stores it as PENDING_APPROVAL and changes nothing until an admin
// decides.
type LearningSuggestion struct {
	TenantID           string        `json:"tenantId"`
	RuleID             string        `json:"ruleId,omitempty"`
	EventPattern       *EventPattern `json:"eventPattern,omitempty"`
	CurrentAdjustment  int           `json:"currentAdjustment"`
	ProposedAdjustment int           `json:"proposedAdjustment"`
	SignalCount        int           `json:"signalCount"`
	Rationale          string        `json:"rationale"`
}

// SubmitLearningSuggestion submits a suggestion and returns the ID AgentPulse
// gave it. AgentPulse has no idempotency key for suggestions, so the caller
// must not submit one twice.
func (c *Client) SubmitLearningSuggestion(ctx context.Context, s LearningSuggestion) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, pathSuggestions, s, &out)
	return out.ID, err
}

// Suggestion is one of a tenant's learning suggestions as AgentPulse lists
// it. Body is the suggestion as submitted; AgentPulse's own suggestions may
// leave some of its fields empty.
type Suggestion struct {
	ID     string             `json:"id"`
	Status string             `json:"status"`
	Body   LearningSuggestion `json:"suggestion"`
}

// Suggestion statuses.
const (
	SuggestionPending  = "PENDING_APPROVAL"
	SuggestionApproved = "APPROVED"
	SuggestionRejected = "REJECTED"
)

// LearningSuggestions returns the tenant's suggestions, decoded.
func (c *Client) LearningSuggestions(ctx context.Context, tenantID string) ([]Suggestion, error) {
	var out struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	err := c.do(ctx, http.MethodGet, pathSuggestions+"?tenantId="+url.QueryEscape(tenantID), nil, &out)
	return out.Suggestions, err
}

// FeedbackGroup is AgentPulse's count of notifications and feedback signals
// for one event type and one payload label (R7). A nil Label counts the
// notifications that carry no label. DistinctUsers is how many users the
// group's signals came from.
type FeedbackGroup struct {
	EventType     string  `json:"eventType"`
	Label         *string `json:"label"`
	Delivered     int     `json:"delivered"`
	Acted         int     `json:"acted"`
	Dismissed     int     `json:"dismissed"`
	Muted         int     `json:"muted"`
	Snoozed       int     `json:"snoozed"`
	DistinctUsers int     `json:"distinctUsers"`
}

// FeedbackAnalytics is a tenant's feedback counts since a time (R7).
type FeedbackAnalytics struct {
	Since  time.Time       `json:"since"`
	Groups []FeedbackGroup `json:"groups"`
}

// FeedbackAnalytics returns the tenant's feedback counts per event type and
// label since the given time (G16, R7). since is sent in RFC 3339, in UTC,
// to the second.
func (c *Client) FeedbackAnalytics(ctx context.Context, tenantID string, since time.Time) (FeedbackAnalytics, error) {
	var out FeedbackAnalytics
	path := pathAnalytics + "?tenantId=" + url.QueryEscape(tenantID) + "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// InAppFeed returns one page of a user's in-app feed, newest first, as
// AgentPulse sends it: {"notifications": [...], "nextCursor"}. A limit of 0
// leaves the page size to AgentPulse, and an empty cursor starts at the
// newest.
func (c *Client) InAppFeed(ctx context.Context, tenantID, userID string, limit int, cursor string) (json.RawMessage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	path := userPath(tenantID, userID) + "/in-app-feed"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// Contact is how AgentPulse reaches a user. PUT replaces it whole: a nil
// field is cleared, a contact with all three nil is deleted, and sending the
// same contact again changes nothing. Every field is always sent.
type Contact struct {
	Email       *string `json:"email"`
	SlackUserID *string `json:"slackUserId"`
	Phone       *string `json:"phone"`
}

// PutContact replaces the user's contact at AgentPulse.
func (c *Client) PutContact(ctx context.Context, tenantID, userID string, ct Contact) error {
	return c.do(ctx, http.MethodPut, userPath(tenantID, userID)+"/contact", ct, nil)
}

// userPath is /v1/tenants/{t}/users/{u}, each part escaped.
func userPath(tenantID, userID string) string {
	return pathTenants + url.PathEscape(tenantID) + "/users/" + url.PathEscape(userID)
}

const maxErrorBody = 200

func (c *Client) do(ctx context.Context, method, path string, in, out any) (err error) {
	if c.breaker != nil {
		if !c.breaker.Allow() {
			return ErrCircuitOpen
		}
		defer func() { c.breaker.Record(countsAsFailure(ctx, err)) }()
	}

	var body io.Reader
	switch v := in.(type) {
	case nil:
	case json.RawMessage:
		body = bytes.NewReader(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("agentpulse: encode %s: %w", path, err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("agentpulse: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", "taskify")

	resp, err := c.http.Do(req)
	if err != nil {
		// The URL holds no secrets; the API key travels in a header.
		return fmt.Errorf("agentpulse: %s %s: %w", method, pathOnly(path), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &StatusError{Method: method, Path: pathOnly(path), Code: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("agentpulse: decode %s: %w", pathOnly(path), err)
	}
	return nil
}

// countsAsFailure: 5xx and transport errors count against AgentPulse. A 4xx
// is the caller's fault, and a cancelled caller says nothing about AgentPulse.
func countsAsFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	// Transport errors and client timeouts.
	return true
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
