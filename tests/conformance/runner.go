package conformance

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rtrtarun/taskify/contract"
	"github.com/rtrtarun/taskify/contract/contracttest"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
	"github.com/rtrtarun/taskify/tests/mockllm"
)

// Config is what the runner needs to know about the target.
type Config struct {
	// TargetURL is the server under test, such as http://127.0.0.1:8091.
	TargetURL string
	// JWTSecret is the target's JWT_SECRET: the runner signs HS256 tokens
	// with it, as cmd/devtoken does.
	JWTSecret string
	// WebhookSecret is the target's AGENTPULSE_WEBHOOK_SECRET.
	WebhookSecret string
}

// Runner runs fixtures against one target. Fixtures run one at a time: the
// mocks are shared.
type Runner struct {
	cfg       Config
	http      *http.Client
	ap        *mockagentpulse.Server
	llm       *mockllm.Server
	validator *contracttest.Validator
}

// NewRunner returns a runner that drives cfg.TargetURL and observes the
// given mocks, which the target must be configured to call.
func NewRunner(cfg Config, ap *mockagentpulse.Server, llm *mockllm.Server) (*Runner, error) {
	v, err := contracttest.NewValidator()
	if err != nil {
		return nil, err
	}
	cfg.TargetURL = strings.TrimRight(cfg.TargetURL, "/")
	return &Runner{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, ap: ap, llm: llm, validator: v}, nil
}

// run is one fixture's state.
type run struct {
	*Runner
	t      *testing.T
	f      *Fixture
	vars   vars
	tenant string
	other  string
	// lastWebhook is the previous webhook sent, for ReplayPrevious.
	lastWebhook *sentWebhook
	// notifyStart is where this fixture's notifications start in the
	// mock's log, to check them against contract/events at the end.
	notifyStart int
}

type sentWebhook struct {
	body    []byte
	headers map[string]string
}

// Run executes one fixture in a fresh tenant.
func (r *Runner) Run(t *testing.T, f *Fixture) {
	t.Helper()
	suffix := make([]byte, 5)
	_, _ = rand.Read(suffix)
	tenant := "conf-" + hex.EncodeToString(suffix)
	x := &run{Runner: r, t: t, f: f, tenant: tenant, other: tenant + "-other",
		vars: newVars(tenant, tenant+"-other", time.Now()), notifyStart: len(r.ap.RequestsTo("POST", "/v1/notify"))}
	x.setupWorkspace()
	for i, s := range f.Steps {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("step %d", i+1)
		}
		if !x.step(name, s) {
			return
		}
	}
	x.checkEvents()
}

func (x *run) setupWorkspace() {
	ws := x.f.Workspace
	if ws != nil && ws.Skip {
		return
	}
	admin := identity{user: "usr_admin", role: "admin", tenant: x.tenant}
	status, body := x.call(admin, "POST", "/api/v1/workspaces", map[string]any{"id": "ws_conf", "name": "Conformance"})
	if status != http.StatusCreated {
		x.t.Fatalf("set up the workspace: %d %s", status, body)
	}
	if ws != nil && ws.AgentSettings != nil {
		status, body := x.call(admin, "PATCH", "/api/v1/workspaces/ws_conf/settings", map[string]any{"agent_settings": ws.AgentSettings})
		if status != http.StatusOK {
			x.t.Fatalf("set up the agent settings: %d %s", status, body)
		}
	}
}

// call is a request the runner makes for itself, checked against the
// contract like any other.
func (x *run) call(as identity, method, path string, body any) (int, []byte) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	resp, err := x.send(as, method, path, nil, raw)
	if err != nil {
		x.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp.status, resp.body
}

type response struct {
	status int
	header http.Header
	body   []byte
	doc    any
}

// send makes one request, retrying on 429 a few times, and checks the
// response against openapi.yaml.
func (x *run) send(as identity, method, path string, headers map[string]string, body []byte) (*response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, x.cfg.TargetURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := x.authenticate(req, as); err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := x.http.Do(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			time.Sleep(time.Second)
			continue
		}
		if err := x.validator.Check(req, resp.StatusCode, resp.Header, raw); err != nil {
			x.t.Errorf("contract: %v", err)
		}
		out := &response{status: resp.StatusCode, header: resp.Header, body: raw}
		if len(bytes.TrimSpace(raw)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			_ = dec.Decode(&out.doc)
		}
		return out, nil
	}
}

// --- identities ----------------------------------------------------------------

type identity struct {
	kind   string // "", none, expired, bad_signature, malformed, headers
	user   string
	role   string
	tenant string // a template; "" is the fixture's tenant
}

func parseIdentity(raw json.RawMessage) (identity, error) {
	if len(raw) == 0 {
		return identity{user: "usr_bob", role: "member"}, nil
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch name {
		case "", "member":
			return identity{user: "usr_bob", role: "member"}, nil
		case "admin":
			return identity{user: "usr_admin", role: "admin"}, nil
		case "other_tenant":
			return identity{user: "usr_eve", role: "admin", tenant: "{{other_tenant}}"}, nil
		case "none", "malformed":
			return identity{kind: name}, nil
		case "expired", "bad_signature", "headers":
			return identity{kind: name, user: "usr_bob", role: "member"}, nil
		}
		return identity{}, fmt.Errorf("unknown identity %q", name)
	}
	var obj struct {
		User   string `json:"user"`
		Role   string `json:"role"`
		Tenant string `json:"tenant"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil || obj.User == "" {
		return identity{}, fmt.Errorf("as must name an identity or be {user, role, tenant}")
	}
	if obj.Role == "" {
		obj.Role = "member"
	}
	return identity{user: obj.User, role: obj.Role, tenant: obj.Tenant}, nil
}

// authenticate adds the identity's credentials: an HS256 token signed with
// the target's JWT_SECRET, like cmd/devtoken's, or the identity headers.
func (x *run) authenticate(req *http.Request, id identity) error {
	tenant := x.tenant
	if id.tenant != "" {
		t, err := x.vars.expandString(id.tenant)
		if err != nil {
			return err
		}
		tenant = t
	}
	exp, secret := time.Now().Add(time.Hour), x.cfg.JWTSecret
	switch id.kind {
	case "none":
		return nil
	case "malformed":
		req.Header.Set("Authorization", "Bearer not-a-token")
		return nil
	case "headers":
		req.Header.Set("X-Tenant-ID", tenant)
		req.Header.Set("X-User-ID", id.user)
		req.Header.Set("X-User-Role", id.role)
		return nil
	case "expired":
		exp = time.Now().Add(-time.Hour)
	case "bad_signature":
		secret = "not-the-server-secret"
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": id.user, "tenant_id": tenant, "role": id.role, "exp": exp.Unix(), "iat": time.Now().Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// --- steps ----------------------------------------------------------------------

// step runs one step and reports whether the fixture may go on.
func (x *run) step(name string, s Step) bool {
	t := x.t
	fail := func(format string, a ...any) bool {
		t.Errorf("%s: %s", name, fmt.Sprintf(format, a...))
		return false
	}
	as, _ := parseIdentity(s.As)
	// Earlier steps' rows are sent first, so what this step causes is
	// told apart from what they caused.
	x.quiesce()
	if err := x.prepareMocks(s.Mock); err != nil {
		return fail("mock: %v", err)
	}
	outboxBefore, err := x.outboxIDs()
	if err != nil {
		return fail("read the outbox: %v", err)
	}
	apBefore := x.agentPulseMarks()

	timeout := 0 * time.Second
	if s.Eventually != nil {
		timeout = seconds(s.Eventually.TimeoutSeconds, 30)
	}
	deadline := time.Now().Add(timeout)
	var resp *response
	for {
		var err error
		if s.Webhook != nil {
			resp, err = x.sendWebhook(s.Webhook)
		} else {
			resp, err = x.sendRequest(as, s.Request)
		}
		if err != nil {
			return fail("%v", err)
		}
		err = x.checkResponse(s.Expect, resp)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fail("%v\nresponse: %d %s", err, resp.status, truncate(resp.body))
		}
		time.Sleep(250 * time.Millisecond)
	}
	for name, path := range s.Capture {
		v, ok := lookup(resp.doc, path)
		if !ok {
			return fail("capture %s: no %s in %s", name, path, truncate(resp.body))
		}
		x.vars[name] = v
	}
	if s.Outbox != nil {
		if err := x.checkOutbox(s.Outbox, outboxBefore); err != nil {
			return fail("outbox: %v", err)
		}
	}
	if s.AgentPulse != nil {
		if err := x.checkAgentPulse(s.AgentPulse, apBefore); err != nil {
			return fail("agentpulse: %v", err)
		}
	}
	return true
}

func seconds(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}

func (x *run) prepareMocks(m *MockSetup) error {
	if m == nil {
		return nil
	}
	for _, f := range m.AgentPulseFail {
		x.ap.FailNext(f.Method, f.Path, f.Status, f.Count)
	}
	for _, raw := range m.LLM {
		doc, err := x.vars.expandJSON(raw)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(doc)
		var sc mockllm.Script
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sc); err != nil {
			return fmt.Errorf("llm script: %w", err)
		}
		if sc.Match == "" {
			return errors.New("an llm script needs a match")
		}
		x.llm.Add(sc)
	}
	return nil
}

func (x *run) sendRequest(as identity, r *Request) (*response, error) {
	path, err := x.vars.expandString(r.Path)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, v := range r.Headers {
		if headers[k], err = x.vars.expandString(v); err != nil {
			return nil, err
		}
	}
	var body []byte
	switch {
	case r.RawBody != nil:
		body = []byte(*r.RawBody)
	case len(r.Body) > 0:
		doc, err := x.vars.expandJSON(r.Body)
		if err != nil {
			return nil, err
		}
		body, _ = json.Marshal(doc)
	}
	return x.send(as, r.Method, path, headers, body)
}

// sendWebhook signs and sends an AgentPulse callback (section 5.5): the
// signature is the hex HMAC-SHA256 of "{timestamp}.{body}".
func (x *run) sendWebhook(w *Webhook) (*response, error) {
	if w.ReplayPrevious {
		if x.lastWebhook == nil {
			return nil, errors.New("replay_previous: no webhook was sent before")
		}
		return x.send(identity{kind: "none"}, "POST", "/api/v1/webhooks/agentpulse", x.lastWebhook.headers, x.lastWebhook.body)
	}
	doc, err := x.vars.expandJSON(w.Body)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(doc)
	ts := strconv.FormatInt(time.Now().Unix()+int64(w.TimestampOffset), 10)
	headers := map[string]string{"X-AgentPulse-Timestamp": ts}
	switch w.Signature {
	case "", "valid":
		headers["X-AgentPulse-Signature"] = "sha256=" + sign(x.cfg.WebhookSecret, ts, body)
	case "wrong":
		headers["X-AgentPulse-Signature"] = "sha256=" + sign("not-the-shared-secret", ts, body)
	case "malformed":
		headers["X-AgentPulse-Signature"] = "sha256=zz"
	case "missing":
	default:
		return nil, fmt.Errorf("unknown signature %q", w.Signature)
	}
	x.lastWebhook = &sentWebhook{body: body, headers: headers}
	return x.send(identity{kind: "none"}, "POST", "/api/v1/webhooks/agentpulse", headers, body)
}

func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (x *run) checkResponse(e *Expect, resp *response) error {
	if resp.status != e.Status {
		return fmt.Errorf("status %d, want %d", resp.status, e.Status)
	}
	for k, v := range e.Headers {
		want, err := x.vars.expandString(v)
		if err != nil {
			return err
		}
		if got := resp.header.Get(k); got != want {
			return fmt.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if e.ErrorCode != "" {
		if code, _ := lookup(resp.doc, "error.code"); code != e.ErrorCode {
			return fmt.Errorf("error code %v, want %s", code, e.ErrorCode)
		}
	}
	if len(e.Body) > 0 {
		pattern, err := x.vars.expandJSON(e.Body)
		if err != nil {
			return err
		}
		if err := match(pattern, resp.doc, ""); err != nil {
			return fmt.Errorf("body: %w", err)
		}
	}
	return nil
}

// --- outbox ------------------------------------------------------------------------

// outboxRows lists every outbox row of the tenant through the admin API.
func (x *run) outboxRows() ([]map[string]any, error) {
	admin := identity{user: "usr_conformance", role: "admin"}
	var out []map[string]any
	path := "/api/v1/admin/outbox?limit=200"
	for {
		resp, err := x.send(admin, "GET", path, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.status != http.StatusOK {
			return nil, fmt.Errorf("GET /admin/outbox: %d %s", resp.status, truncate(resp.body))
		}
		entries, _ := lookup(resp.doc, "entries")
		list, _ := entries.([]any)
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		next, _ := lookup(resp.doc, "next_cursor")
		cursor, ok := next.(string)
		if !ok || cursor == "" {
			return out, nil
		}
		path = "/api/v1/admin/outbox?limit=200&cursor=" + cursor
	}
}

func (x *run) outboxIDs() (map[string]bool, error) {
	rows, err := x.outboxRows()
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, r := range rows {
		ids[text(r["id"])] = true
	}
	return ids, nil
}

// checkOutbox waits until the rows written since the step began match.
func (x *run) checkOutbox(e *OutboxExpect, before map[string]bool) error {
	deadline := time.Now().Add(seconds(e.TimeoutSeconds, 20))
	for {
		rows, err := x.outboxRows()
		if err != nil {
			return err
		}
		var fresh []any
		for _, r := range rows {
			if before[text(r["id"])] {
				continue
			}
			// Recipient is the payload's userId, for every row type.
			if p, ok := r["payload"].(map[string]any); ok {
				r["recipient"] = p["userId"]
			}
			fresh = append(fresh, r)
		}
		patterns := make([]any, len(e.Rows))
		for i, row := range e.Rows {
			p := map[string]any{"event_type": row.EventType}
			for k, v := range map[string]string{"idempotency_key": row.IdempotencyKey, "recipient": row.Recipient, "status": row.Status} {
				if v == "" {
					continue
				}
				s, err := x.vars.expandString(v)
				if err != nil {
					return err
				}
				p[k] = s
			}
			if row.Attempts != nil {
				p["attempts"] = float64(*row.Attempts)
			}
			patterns[i] = p
		}
		err = matchSet(patterns, numbersToFloat(fresh), !e.Includes)
		if err == nil {
			return x.captureRows(e, fresh)
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (x *run) captureRows(e *OutboxExpect, rows []any) error {
	for i, row := range e.Rows {
		if len(row.Capture) == 0 {
			continue
		}
		for _, r := range rows {
			m := r.(map[string]any)
			if m["event_type"] != row.EventType {
				continue
			}
			if row.Recipient != "" {
				want, _ := x.vars.expandString(row.Recipient)
				if m["recipient"] != want {
					continue
				}
			}
			for name, path := range row.Capture {
				v, ok := lookup(m, path)
				if !ok {
					return fmt.Errorf("row %d: no %s to capture", i, path)
				}
				x.vars[name] = v
			}
			break
		}
	}
	return nil
}

// numbersToFloat makes rows read from JSON with UseNumber comparable.
func numbersToFloat(items []any) []any {
	b, _ := json.Marshal(items)
	var out []any
	_ = json.Unmarshal(b, &out)
	return out
}

// --- AgentPulse --------------------------------------------------------------------

type apMarks struct{ notifications, notifyCalls, feedback int }

func (x *run) agentPulseMarks() apMarks {
	return apMarks{len(x.ap.Notifications()), len(x.ap.RequestsTo("POST", "/v1/notify")), len(x.ap.Feedback())}
}

func (x *run) checkAgentPulse(e *AgentPulseExpect, from apMarks) error {
	deadline := time.Now().Add(seconds(e.TimeoutSeconds, 20))
	for {
		err := x.agentPulseMatches(e, from)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (x *run) agentPulseMatches(e *AgentPulseExpect, from apMarks) error {
	mine := func(body map[string]any) bool { return body["tenantId"] == x.tenant }
	if e.Notifications != nil {
		var got []any
		for _, n := range x.ap.Notifications()[from.notifications:] {
			if mine(n.Body) {
				got = append(got, n.Body)
			}
		}
		if err := x.matchPatterns(e.Notifications, got); err != nil {
			return fmt.Errorf("notifications: %w", err)
		}
	}
	if e.NotifyCalls != nil {
		n := 0
		for _, r := range x.ap.RequestsTo("POST", "/v1/notify")[from.notifyCalls:] {
			var body map[string]any
			if r.JSON(&body) == nil && mine(body) {
				n++
			}
		}
		if n != *e.NotifyCalls {
			return fmt.Errorf("%d calls to /v1/notify, want %d", n, *e.NotifyCalls)
		}
	}
	if e.Feedback != nil {
		var got []any
		for _, f := range x.ap.Feedback()[from.feedback:] {
			if mine(f) {
				got = append(got, f)
			}
		}
		if err := x.matchPatterns(e.Feedback, got); err != nil {
			return fmt.Errorf("feedback: %w", err)
		}
	}
	return nil
}

func (x *run) matchPatterns(raw []json.RawMessage, got []any) error {
	patterns := make([]any, len(raw))
	for i, r := range raw {
		p, err := x.vars.expandJSON(r)
		if err != nil {
			return err
		}
		patterns[i] = p
	}
	return matchSet(patterns, numbersToFloat(got), true)
}

// checkEvents validates every /v1/notify body the fixture caused against
// contract/events (M12-02).
func (x *run) checkEvents() {
	for _, r := range x.ap.RequestsTo("POST", "/v1/notify")[x.notifyStart:] {
		var head struct {
			TenantID  string `json:"tenantId"`
			EventType string `json:"eventType"`
		}
		if err := json.Unmarshal(r.Body, &head); err != nil || head.TenantID != x.tenant {
			continue
		}
		if err := contract.ValidateEvent(head.EventType, r.Body); err != nil {
			x.t.Errorf("event: %v", err)
		}
	}
}

func truncate(b []byte) string {
	if len(b) > 1500 {
		return string(b[:1500]) + "..."
	}
	return string(b)
}

// quiesce waits, up to 15 seconds, until the tenant has no outbox row that
// is pending or being sent. A row still retrying after that is left to the
// step that expects it.
func (x *run) quiesce() {
	admin := identity{user: "usr_conformance", role: "admin"}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, status := range []string{"pending", "processing"} {
			resp, err := x.send(admin, "GET", "/api/v1/admin/outbox?limit=1&status="+status, nil, nil)
			if err != nil || resp.status != http.StatusOK {
				return
			}
			if entries, _ := lookup(resp.doc, "entries"); len(entries.([]any)) > 0 {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
