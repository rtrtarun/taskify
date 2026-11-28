// Package conformance is the black-box conformance runner (docs/architecture.md
// section 9.3, M12-06). It reads the fixtures in contract/fixtures and runs
// them against the server at TARGET_URL, with its own mock AgentPulse and
// mock LLM, and checks every response against contract/openapi.yaml and
// every notification against contract/events.
//
// It imports no server package and never reads the database: outbox effects
// are observed through GET /api/v1/admin/outbox and through what the mock
// AgentPulse received. contract/fixtures/README.md describes the fixture
// format and how to start a target.
package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Fixture is one contract/fixtures/**/*.json file: steps run in order
// against a fresh tenant.
type Fixture struct {
	Description string `json:"description"`
	// Rules names the contract/RULES.md rules the fixture encodes.
	Rules []string `json:"rules,omitempty"`
	// Workspace sets up the tenant's workspace ws_conf. Absent: created with
	// the default settings.
	Workspace *WorkspaceSetup `json:"workspace,omitempty"`
	Steps     []Step          `json:"steps"`

	// Name is the path below contract/fixtures, without .json.
	Name string `json:"-"`
}

// WorkspaceSetup is how the runner prepares the fixture's workspace.
type WorkspaceSetup struct {
	// Skip leaves the tenant without a workspace.
	Skip bool `json:"skip,omitempty"`
	// AgentSettings, when set, is sent as the workspace's agent_settings
	// after it is created.
	AgentSettings map[string]any `json:"agent_settings,omitempty"`
}

// Step is one request and what must follow from it.
type Step struct {
	Name string `json:"name,omitempty"`
	// As is who sends the request: "member" (the default, usr_bob),
	// "admin" (usr_admin), "other_tenant" (an admin of another tenant),
	// "none", "expired", "bad_signature", "malformed", "headers" (the
	// development identity headers instead of a token), or an object
	// {"user", "role", "tenant"}.
	As json.RawMessage `json:"as,omitempty"`
	// Mock scripts the mocks before the request.
	Mock *MockSetup `json:"mock,omitempty"`
	// Request or Webhook: exactly one.
	Request *Request `json:"request,omitempty"`
	Webhook *Webhook `json:"webhook,omitempty"`
	Expect  *Expect  `json:"expect,omitempty"`
	// Eventually repeats the request until Expect holds, for effects that
	// happen in the background, such as an agent's run.
	Eventually *Wait `json:"eventually,omitempty"`
	// Capture stores values of the response body under names, for later
	// steps: {"task": "id"}; a path may index lists, as "entries.0.id".
	Capture map[string]string `json:"capture,omitempty"`
	// Outbox lists the outbox rows the step must have written, observed
	// through GET /api/v1/admin/outbox.
	Outbox *OutboxExpect `json:"outbox,omitempty"`
	// AgentPulse lists what the mock AgentPulse must have received for the
	// tenant since the step began.
	AgentPulse *AgentPulseExpect `json:"agentpulse,omitempty"`
}

// MockSetup scripts the mocks.
type MockSetup struct {
	// AgentPulseFail makes the mock answer the next Count calls to Method
	// Path with Status.
	AgentPulseFail []Failure `json:"agentpulse_fail,omitempty"`
	// LLM adds scripts to the mock LLM: {"match", "answers", "repeat"}, as
	// tests/mockllm.Script, templated.
	LLM []json.RawMessage `json:"llm,omitempty"`
}

// Failure is a scripted AgentPulse failure.
type Failure struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Count  int    `json:"count"`
}

// Request is an API call. Path, header values, and string values in Body
// are templates.
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	// RawBody is sent as it is, for bodies that are not valid JSON.
	RawBody *string `json:"raw_body,omitempty"`
}

// Webhook is a callback from AgentPulse, signed by the runner.
type Webhook struct {
	Body json.RawMessage `json:"body,omitempty"`
	// TimestampOffset moves X-AgentPulse-Timestamp from now, in seconds.
	TimestampOffset int `json:"timestamp_offset_seconds,omitempty"`
	// Signature is "valid" (the default), "wrong" (another secret),
	// "malformed", or "missing".
	Signature string `json:"signature,omitempty"`
	// ReplayPrevious sends the previous webhook again, byte for byte, with
	// the same timestamp and signature.
	ReplayPrevious bool `json:"replay_previous,omitempty"`
}

// Expect is what the response must be. Body is a pattern (see match).
type Expect struct {
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      json.RawMessage   `json:"body,omitempty"`
	ErrorCode string            `json:"error_code,omitempty"`
}

// Wait bounds an Eventually.
type Wait struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// OutboxExpect is the set of rows the step wrote in the tenant: exactly
// these, in any order, unless Includes.
type OutboxExpect struct {
	Rows           []OutboxRow `json:"rows"`
	Includes       bool        `json:"includes,omitempty"`
	TimeoutSeconds int         `json:"timeout_seconds,omitempty"`
}

// OutboxRow describes one row. Recipient is payload.userId. An empty field
// matches anything; Status is waited for.
type OutboxRow struct {
	EventType      string            `json:"event_type"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Recipient      string            `json:"recipient,omitempty"`
	Status         string            `json:"status,omitempty"`
	Attempts       *int              `json:"attempts,omitempty"`
	Capture        map[string]string `json:"capture,omitempty"`
}

// AgentPulseExpect is what the mock AgentPulse received for the tenant.
type AgentPulseExpect struct {
	// Notifications are the accepted /v1/notify bodies, as patterns:
	// exactly these, in any order.
	Notifications []json.RawMessage `json:"notifications,omitempty"`
	// NotifyCalls counts every /v1/notify call, failed ones included.
	NotifyCalls *int `json:"notify_calls,omitempty"`
	// Feedback are the /v1/feedback bodies, as patterns: exactly these.
	Feedback       []json.RawMessage `json:"feedback,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// LoadFixtures reads every fixture under dir, strictly: an unknown field is
// an error, so a typo cannot silently weaken a fixture.
func LoadFixtures(dir string) ([]*Fixture, error) {
	var out []*Fixture
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f := &Fixture{}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(f); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		rel, _ := filepath.Rel(dir, path)
		f.Name = strings.TrimSuffix(filepath.ToSlash(rel), ".json")
		if err := f.check(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, f)
		return nil
	})
	slices.SortFunc(out, func(a, b *Fixture) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

func (f *Fixture) check() error {
	if strings.TrimSpace(f.Description) == "" {
		return fmt.Errorf("a fixture needs a description")
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("a fixture needs steps")
	}
	for i, s := range f.Steps {
		switch {
		case (s.Request == nil) == (s.Webhook == nil):
			return fmt.Errorf("step %d: give exactly one of request and webhook", i+1)
		case s.Expect == nil || s.Expect.Status == 0:
			return fmt.Errorf("step %d: expect.status is required", i+1)
		case s.Request != nil && (s.Request.Method == "" || !strings.HasPrefix(s.Request.Path, "/")):
			return fmt.Errorf("step %d: a request needs a method and an absolute path", i+1)
		}
		if _, err := parseIdentity(s.As); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		for _, raw := range [][]byte{s.Expect.Body} {
			if len(raw) > 0 && !json.Valid(raw) {
				return fmt.Errorf("step %d: expect.body is not JSON", i+1)
			}
		}
	}
	return nil
}

// --- templates --------------------------------------------------------------

var placeholder = regexp.MustCompile(`\{\{([a-z0-9_]+)\}\}`)

// vars holds a fixture's variables: built-ins and captures.
type vars map[string]any

// newVars returns the built-in variables of a run starting at now.
func newVars(tenant, other string, now time.Time) vars {
	v := vars{"tenant": tenant, "other_tenant": other, "workspace": "ws_conf", "random_uuid": uuid.NewString()}
	now = now.UTC().Truncate(time.Second)
	for name, d := range map[string]time.Duration{
		"now": 0, "in_1h": time.Hour, "in_2h": 2 * time.Hour, "in_30h": 30 * time.Hour, "in_48h": 48 * time.Hour,
		"ago_1h": -time.Hour, "ago_26h": -26 * time.Hour,
	} {
		at := now.Add(d)
		v[name] = at.Format(time.RFC3339)
		v[name+"_unix"] = at.Unix()
		v[name+"_date"] = at.Format(time.DateOnly)
	}
	return v
}

// expand replaces {{name}} in s. A string that is one placeholder and
// nothing else becomes the variable's value with its JSON type.
func (v vars) expand(s string) (any, error) {
	if m := placeholder.FindStringSubmatch(s); m != nil && m[0] == s {
		val, ok := v[m[1]]
		if !ok {
			return nil, fmt.Errorf("unknown variable {{%s}}", m[1])
		}
		return val, nil
	}
	var missing string
	out := placeholder.ReplaceAllStringFunc(s, func(p string) string {
		name := placeholder.FindStringSubmatch(p)[1]
		val, ok := v[name]
		if !ok {
			missing = name
			return p
		}
		return text(val)
	})
	if missing != "" {
		return nil, fmt.Errorf("unknown variable {{%s}}", missing)
	}
	return out, nil
}

// expandString is expand for places that need text.
func (v vars) expandString(s string) (string, error) {
	val, err := v.expand(s)
	if err != nil {
		return "", err
	}
	return text(val), nil
}

// expandJSON expands every string in a JSON document.
func (v vars) expandJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return v.walk(doc)
}

func (v vars) walk(doc any) (any, error) {
	switch x := doc.(type) {
	case string:
		return v.expand(x)
	case []any:
		for i, item := range x {
			val, err := v.walk(item)
			if err != nil {
				return nil, err
			}
			x[i] = val
		}
	case map[string]any:
		for k, item := range x {
			val, err := v.walk(item)
			if err != nil {
				return nil, err
			}
			x[k] = val
		}
	}
	return doc, nil
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case nil:
		return "null"
	}
	return fmt.Sprint(v)
}

// lookup follows a dotted path, with list indexes, into a decoded body.
func lookup(doc any, path string) (any, bool) {
	cur := doc
	for _, part := range strings.Split(path, ".") {
		switch x := cur.(type) {
		case map[string]any:
			next, ok := x[part]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// --- patterns -----------------------------------------------------------------

var (
	uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	tsRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
)

// match checks a decoded value against a pattern. Objects match when every
// key of the pattern matches (others are ignored); "$absent" requires a key
// to be missing. Lists match element by element and must have the same
// length, unless the pattern is {"$length": n} or {"$contains": [...]}.
// Strings "$any", "$uuid", "$timestamp", and "$nonempty" match by kind;
// {"$one_of": [...]} matches any of its values. Numbers compare by value.
func match(pattern, actual any, at string) error {
	fail := func(format string, a ...any) error {
		if at == "" {
			at = "/"
		}
		return fmt.Errorf("at %s: %s", at, fmt.Sprintf(format, a...))
	}
	switch p := pattern.(type) {
	case string:
		s, isStr := actual.(string)
		switch p {
		case "$any":
			return nil
		case "$absent":
			return fail("must be absent")
		case "$uuid":
			if !isStr || !uuidRe.MatchString(s) {
				return fail("want a UUID, got %v", actual)
			}
			return nil
		case "$timestamp":
			if !isStr || !tsRe.MatchString(s) {
				return fail("want an RFC 3339 UTC timestamp, got %v", actual)
			}
			return nil
		case "$nonempty":
			if !isStr || s == "" {
				return fail("want a non-empty string, got %v", actual)
			}
			return nil
		}
		if !isStr || s != p {
			return fail("want %q, got %v", p, show(actual))
		}
		return nil
	case json.Number, float64, int, int64:
		want, _ := strconv.ParseFloat(text(p), 64)
		got, ok := number(actual)
		if !ok || got != want {
			return fail("want %v, got %v", text(p), show(actual))
		}
		return nil
	case bool:
		if b, ok := actual.(bool); !ok || b != p {
			return fail("want %v, got %v", p, show(actual))
		}
		return nil
	case nil:
		if actual != nil {
			return fail("want null, got %v", show(actual))
		}
		return nil
	case []any:
		list, ok := actual.([]any)
		if !ok {
			return fail("want a list, got %v", show(actual))
		}
		if len(list) != len(p) {
			return fail("want %d items, got %d: %v", len(p), len(list), show(actual))
		}
		for i := range p {
			if err := match(p[i], list[i], fmt.Sprintf("%s/%d", at, i)); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if n, ok := p["$length"]; ok && len(p) == 1 {
			list, isList := actual.([]any)
			want, _ := number(n)
			if !isList || float64(len(list)) != want {
				return fail("want %v items, got %v", text(n), show(actual))
			}
			return nil
		}
		if items, ok := p["$contains"].([]any); ok && len(p) == 1 {
			list, isList := actual.([]any)
			if !isList {
				return fail("want a list, got %v", show(actual))
			}
			for i, item := range items {
				if !slices.ContainsFunc(list, func(a any) bool { return match(item, a, "") == nil }) {
					return fail("no item matches pattern %d: %v", i, show(item))
				}
			}
			return nil
		}
		if options, ok := p["$one_of"].([]any); ok && len(p) == 1 {
			for _, o := range options {
				if match(o, actual, at) == nil {
					return nil
				}
			}
			return fail("want one of %v, got %v", show(options), show(actual))
		}
		obj, ok := actual.(map[string]any)
		if !ok {
			return fail("want an object, got %v", show(actual))
		}
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			got, present := obj[k]
			if p[k] == "$absent" {
				if present {
					return fail("%s must be absent, got %v", k, show(got))
				}
				continue
			}
			if !present {
				return fail("%s is missing", k)
			}
			if err := match(p[k], got, at+"/"+k); err != nil {
				return err
			}
		}
		return nil
	}
	return fail("unsupported pattern %T", pattern)
}

// matchSet pairs every pattern with a distinct item. With exact, every item
// must be paired too.
func matchSet(patterns, items []any, exact bool) error {
	used := make([]bool, len(items))
	for i, p := range patterns {
		found := false
		last := errors.New("no unpaired item left")
		for j, item := range items {
			if used[j] {
				continue
			}
			if err := match(p, item, ""); err == nil {
				used[j], found = true, true
				break
			} else {
				last = err
			}
		}
		if !found {
			return fmt.Errorf("nothing matches pattern %d %v (closest mismatch: %w); have %v", i, show(p), last, show(items))
		}
	}
	if exact && len(items) != len(patterns) {
		return fmt.Errorf("want exactly %d, have %d: %v", len(patterns), len(items), show(items))
	}
	return nil
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(b) > 600 {
		return string(b[:600]) + "..."
	}
	return string(b)
}
