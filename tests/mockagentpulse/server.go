// Package mockagentpulse is a stand-in for the AgentPulse notification-service.
//
// It implements the /v1/ API that AgentPulse builds in its milestone 40, as
// agreed in docs/blockers.md ("Agreed with AgentPulse"), records every request
// so tests can assert on them, and can be scripted to fail. It enforces the
// validation AgentPulse applies from slice 40b: tenant IDs, the fixed
// actionUrls keys and the IDs each needs, and critical rule conditions; and
// what AgentPulse confirmed for AP-12: feedback names a notification or a
// task, a call naming an object by ID names its tenant and another tenant's
// object is not found, and contact fields have fixed formats. Any later
// change to the contract changes this mock in the same commit.
//
// It must not import any Taskify server package: the conformance suite reuses
// it as a black box.
package mockagentpulse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request is one call the mock received.
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
	At     time.Time
}

// JSON decodes the request body into v.
func (r Request) JSON(v any) error { return json.Unmarshal(r.Body, v) }

// Notification is an accepted /v1/notify call, after deduplication.
type Notification struct {
	ID   string
	Body map[string]any
	// At is when the mock accepted it, by its clock (SetClock).
	At time.Time
}

// signal is one accepted feedback signal, as analytics count it: under the
// notification it names, or, without one, the user's latest notification
// about its taskId (AP-12). notification is empty when there is none: the
// signal is stored but not counted.
type signal struct {
	tenant, user, kind, notification string
	at                               time.Time
}

// Suggestion is a learning suggestion held by the mock.
type Suggestion struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Body   map[string]any `json:"suggestion"`
}

// FeedItem is one entry in a user's in-app feed.
type FeedItem map[string]any

type failure struct {
	status    int
	remaining int
}

type storedRule struct {
	id   string
	body map[string]any
}

// Server is the mock. The zero value is not usable; call New.
type Server struct {
	mu            sync.Mutex
	seq           int
	requests      []Request
	failures      map[string]*failure // key: "METHOD /path"
	notifications []Notification
	byKey         map[string]string // tenantId + "\x00" + idempotencyKey -> notification ID
	duplicates    int
	feedback      []map[string]any
	signals       []signal
	now           func() time.Time
	rules         []storedRule
	suggestions   []*Suggestion
	feeds         map[string][]FeedItem // key: tenant + "/" + user
	contacts      map[string]Contact    // key: tenant + "/" + user
	contactPuts   int
	delays        map[string]time.Duration

	mux *http.ServeMux
}

// New returns a ready mock.
func New() *Server {
	s := &Server{
		failures: map[string]*failure{},
		byKey:    map[string]string{},
		feeds:    map[string][]FeedItem{},
		contacts: map[string]Contact{},
		delays:   map[string]time.Duration{},
		now:      time.Now,
		mux:      http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	s.mux.HandleFunc("POST /v1/notify", s.handleNotify)
	s.mux.HandleFunc("POST /v1/feedback", s.handleFeedback)
	s.mux.HandleFunc("POST /v1/rules", s.handleCreateRule)
	s.mux.HandleFunc("GET /v1/rules", s.handleListRules)
	s.mux.HandleFunc("GET /v1/learning/suggestions", s.handleListSuggestions)
	s.mux.HandleFunc("POST /v1/learning/suggestions", s.handleCreateSuggestion)
	s.mux.HandleFunc("POST /v1/learning/suggestions/{id}/decision", s.handleDecision)
	s.mux.HandleFunc("GET /v1/analytics/feedback", s.handleAnalytics)
	s.mux.HandleFunc("GET /v1/tenants/{tenantId}/users/{userId}/in-app-feed", s.handleFeed)
	s.mux.HandleFunc("PUT /v1/tenants/{tenantId}/users/{userId}/contact", s.handlePutContact)
	return s
}

// ServeHTTP records the request, applies any scripted failure, then routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = io.NopCloser(bytes.NewReader(body))

	s.mu.Lock()
	if r.URL.Path != "/healthz" {
		s.requests = append(s.requests, Request{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   body,
			At:     time.Now(),
		})
	}
	status := 0
	if f := s.failures[r.Method+" "+r.URL.Path]; f != nil && f.remaining != 0 {
		status = f.status
		if f.remaining > 0 {
			f.remaining--
		}
	}
	delay := s.delays[r.Method+" "+r.URL.Path]
	s.mu.Unlock()

	// A slow AgentPulse: the request is recorded, then answered late. A
	// client that gives up meanwhile still leaves the call accepted, as a
	// real server would.
	time.Sleep(delay)
	if status != 0 {
		writeJSON(w, status, map[string]any{"error": "scripted failure"})
		return
	}
	s.mux.ServeHTTP(w, r)
}

// --- the agreed rules ------------------------------------------------------

// TenantPattern is the tenant ID rule AgentPulse enforces. IDs are compared
// case-sensitively and never trimmed, and ReservedTenant is refused.
const TenantPattern = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`

// ReservedTenant is the tenant ID AgentPulse keeps for itself.
const ReservedTenant = "default"

var tenantRe = regexp.MustCompile(TenantPattern)

// CheckTenant returns why id is not an acceptable tenant ID, or nil.
func CheckTenant(id string) error {
	switch {
	case id == ReservedTenant:
		return fmt.Errorf("tenantId %q is reserved", id)
	case !tenantRe.MatchString(id):
		return fmt.Errorf("tenantId %q is invalid: it must match %s", id, TenantPattern)
	}
	return nil
}

// ActionKeys are the actionUrls keys AgentPulse accepts, each mapped to the
// payload field holding the ID its callback carries. view is a plain link and
// needs none.
var ActionKeys = map[string]string{
	"complete":  "taskId",
	"snooze_2h": "taskId",
	"unblock":   "taskId",
	"approve":   "jobId",
	"reject":    "jobId",
	"view":      "",
}

func actionKeyList() string {
	keys := make([]string, 0, len(ActionKeys))
	for k := range ActionKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// MaxIdempotencyKey is the longest key AgentPulse stores.
const MaxIdempotencyKey = 320

// CheckNotify returns why AgentPulse would refuse a /v1/notify body with a
// 400, or nil. The message names the field at fault.
func CheckNotify(body map[string]any) error {
	for _, field := range []string{"idempotencyKey", "tenantId", "userId", "eventType", "title", "body"} {
		if v, _ := body[field].(string); v == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	if err := CheckTenant(body["tenantId"].(string)); err != nil {
		return err
	}
	if key := body["idempotencyKey"].(string); len(key) > MaxIdempotencyKey || !printableASCII(key) {
		return fmt.Errorf("idempotencyKey must be 1 to %d printable ASCII characters", MaxIdempotencyKey)
	}
	raw, present := body["payload"]
	if !present || raw == nil {
		return nil
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("payload must be an object")
	}
	return checkActions(payload)
}

// checkActions applies 40b's action rules: only the fixed keys, each with the
// ID its callback needs, and labels only for those keys. A payload with no
// actionUrls, or only view, is valid.
func checkActions(payload map[string]any) error {
	if raw, present := payload["actionUrls"]; present && raw != nil {
		urls, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("payload.actionUrls must be an object of strings")
		}
		keys := make([]string, 0, len(urls))
		for k := range urls {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			idField, known := ActionKeys[k]
			if !known {
				return fmt.Errorf("payload.actionUrls key %q is not supported; use one of %s", k, actionKeyList())
			}
			if v, isString := urls[k].(string); !isString || v == "" {
				return fmt.Errorf("payload.actionUrls.%s must be a URL string", k)
			}
			if idField == "" {
				continue
			}
			if id, _ := payload[idField].(string); id == "" {
				return fmt.Errorf("payload.actionUrls.%s needs payload.%s", k, idField)
			}
		}
	}
	if raw, present := payload["actionLabels"]; present && raw != nil {
		labels, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("payload.actionLabels must be an object of strings")
		}
		for k, v := range labels {
			if _, known := ActionKeys[k]; !known {
				return fmt.Errorf("payload.actionLabels key %q is not supported; use one of %s", k, actionKeyList())
			}
			if _, isString := v.(string); !isString {
				return fmt.Errorf("payload.actionLabels.%s must be a string", k)
			}
		}
	}
	return nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return s != ""
}

// Contact formats, exactly as AgentPulse validates them (AP-12).
const MaxEmail = 254

var (
	slackUserRe = regexp.MustCompile(`^[UW][A-Z0-9]+$`)
	phoneRe     = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)
)

// CheckContact returns why AgentPulse would refuse a contact PUT body, or
// nil. Each field is null, absent, or a value of its format: email a plain
// address of at most 254 characters with no display name, slackUserId
// ^[UW][A-Z0-9]+$ of at most 32 characters, phone ^\+[1-9][0-9]{1,14}$. One
// bad field refuses the whole PUT, and the message names it.
func CheckContact(body map[string]any) error {
	checks := []struct {
		field, rule string
		ok          func(string) bool
	}{
		{"email", "a plain address of at most 254 characters with no display name", func(v string) bool {
			a, err := mail.ParseAddress(v)
			return err == nil && a.Name == "" && a.Address == v && len(v) <= MaxEmail
		}},
		{"slackUserId", "U or W and then capital letters and digits, at most 32 characters", func(v string) bool {
			return len(v) <= 32 && slackUserRe.MatchString(v)
		}},
		{"phone", "E.164: a plus sign and 2 to 15 digits, the first not 0", phoneRe.MatchString},
	}
	for _, c := range checks {
		switch v := body[c.field].(type) {
		case nil:
		case string:
			if !c.ok(v) {
				// The value is personal data: name the field, not the value.
				return fmt.Errorf("%s must be %s", c.field, c.rule)
			}
		default:
			return fmt.Errorf("%s must be a string or null", c.field)
		}
	}
	return nil
}

// Attention classes a rule may set.
var attentionClasses = []string{"critical", "standard", "background"}

// CheckRule returns why AgentPulse would refuse a /v1/rules body, or nil. A
// critical rule needs a real condition, event types or payload values, in
// every entry: an empty entry would make everything critical.
func CheckRule(body map[string]any) error {
	tenant, _ := body["tenantId"].(string)
	if err := CheckTenant(tenant); err != nil {
		return err
	}
	if name, _ := body["name"].(string); name == "" {
		return fmt.Errorf("name is required")
	}
	class := ""
	if raw, present := body["attentionClass"]; present && raw != nil {
		class, _ = raw.(string)
		if !slices.Contains(attentionClasses, class) {
			return fmt.Errorf("attentionClass must be critical, standard, or background")
		}
	}
	entries, _ := body["matchAny"].([]any)
	if len(entries) == 0 {
		return fmt.Errorf("matchAny needs at least one entry")
	}
	for i, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("matchAny[%d] must be an object", i)
		}
		types, _ := entry["eventTypes"].([]any)
		match, _ := entry["payloadMatch"].(map[string]any)
		if class == "critical" && len(types) == 0 && len(match) == 0 {
			return fmt.Errorf("matchAny[%d] of a critical rule needs eventTypes or payloadMatch", i)
		}
	}
	return nil
}

// --- scripting -------------------------------------------------------------

// FailNext makes the next n calls to method and path answer with status,
// without reaching the handler. A negative n fails every call until Reset or
// ClearFailures.
func (s *Server) FailNext(method, path string, status, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method+" "+path] = &failure{status: status, remaining: n}
}

// SetDelay makes every call to method and path wait d before answering.
// Zero removes the delay.
func (s *Server) SetDelay(method, path string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays[method+" "+path] = d
}

// ClearFailures removes all scripted failures.
func (s *Server) ClearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = map[string]*failure{}
}

// SetClock sets the clock that stamps accepted notifications and feedback.
// Nil restores time.Now.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	s.now = now
}

// SeedNotification stores a notification under id as if /v1/notify had
// accepted it, so feedback and callbacks in tests can name it. body is a
// /v1/notify body and must carry tenantId.
func (s *Server) SeedNotification(id string, body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = append(s.notifications, Notification{ID: id, Body: body, At: s.now()})
}

// SeedSuggestion adds a pending learning suggestion and returns its ID.
func (s *Server) SeedSuggestion(body map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addSuggestionLocked(body).ID
}

// SeedFeed sets the in-app feed returned for a tenant and user, newest
// first.
func (s *Server) SeedFeed(tenantID, userID string, items ...FeedItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feeds[tenantID+"/"+userID] = items
}

// Reset clears all recorded and stored state.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
	s.failures = map[string]*failure{}
	s.notifications = nil
	s.byKey = map[string]string{}
	s.duplicates = 0
	s.feedback = nil
	s.signals = nil
	s.rules = nil
	s.suggestions = nil
	s.feeds = map[string][]FeedItem{}
	s.contacts = map[string]Contact{}
	s.contactPuts = 0
	s.delays = map[string]time.Duration{}
}

// --- inspection ------------------------------------------------------------

// Requests returns every recorded request, including failed and duplicate ones.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// RequestsTo returns recorded requests for one method and path.
func (s *Server) RequestsTo(method, path string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// Notifications returns accepted notifications: one per distinct tenant and
// idempotencyKey, in arrival order.
func (s *Server) Notifications() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notification(nil), s.notifications...)
}

// Duplicates returns how many /v1/notify calls were acknowledged as duplicates.
func (s *Server) Duplicates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicates
}

// Feedback returns the bodies of accepted /v1/feedback calls.
func (s *Server) Feedback() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.feedback...)
}

// Rules returns the bodies of accepted /v1/rules calls.
func (s *Server) Rules() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.rules))
	for i, r := range s.rules {
		out[i] = r.body
	}
	return out
}

// Suggestions returns a copy of the stored learning suggestions.
func (s *Server) Suggestions() []Suggestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Suggestion, len(s.suggestions))
	for i, sg := range s.suggestions {
		out[i] = *sg
	}
	return out
}

// Contact is a user's contact as AgentPulse holds it. An empty field is
// unset.
type Contact struct {
	Email       string
	SlackUserID string
	Phone       string
}

// ContactOf returns the user's contact and whether AgentPulse holds one.
func (s *Server) ContactOf(tenantID, userID string) (Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.contacts[tenantID+"/"+userID]
	return c, ok
}

// ContactPuts returns how many contact replaces were accepted, including
// ones that changed nothing.
func (s *Server) ContactPuts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contactPuts
}

// --- handlers --------------------------------------------------------------

// handlePutContact is a full replace (40a): a field that is null or left out
// is cleared, a contact with all three empty is deleted, and the same body
// again changes nothing.
func (s *Server) handlePutContact(w http.ResponseWriter, r *http.Request) {
	tenant, user := r.PathValue("tenantId"), r.PathValue("userId")
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	if err := CheckContact(body); err != nil {
		badRequest(w, err)
		return
	}
	var c Contact
	c.Email, _ = body["email"].(string)
	c.SlackUserID, _ = body["slackUserId"].(string)
	c.Phone, _ = body["phone"].(string)
	key := tenant + "/" + user
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contactPuts++
	if c == (Contact{}) {
		delete(s.contacts, key)
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
		return
	}
	s.contacts[key] = c
	writeJSON(w, http.StatusOK, map[string]any{"email": c.Email, "slackUserId": c.SlackUserID, "phone": c.Phone})
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	if err := CheckNotify(body); err != nil {
		badRequest(w, err)
		return
	}
	// Keys are unique per tenant, stored as sent.
	key := body["tenantId"].(string) + "\x00" + body["idempotencyKey"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()
	if id, seen := s.byKey[key]; seen {
		s.duplicates++
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "duplicate": true})
		return
	}
	id := s.nextIDLocked("ntf")
	s.byKey[key] = id
	s.notifications = append(s.notifications, Notification{ID: id, Body: body, At: s.now()})
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "duplicate": false})
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	tenant, _ := body["tenantId"].(string)
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	user, _ := body["userId"].(string)
	if user == "" {
		badRequest(w, fmt.Errorf("userId is required"))
		return
	}
	kind, _ := body["signal"].(string)
	switch kind {
	case "acted", "dismissed", "snoozed", "muted":
	default:
		badRequest(w, fmt.Errorf("signal must be acted, dismissed, snoozed, or muted"))
		return
	}
	// AP-12: notificationId is optional, and empty means absent, but the
	// signal must name a notification or a task.
	notification, task := "", ""
	for field, dst := range map[string]*string{"notificationId": &notification, "taskId": &task} {
		switch v := body[field].(type) {
		case nil:
		case string:
			*dst = v
		default:
			badRequest(w, fmt.Errorf("%s must be a string", field))
			return
		}
	}
	if notification == "" && task == "" {
		badRequest(w, fmt.Errorf("notificationId or taskId is required"))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	linked := notification
	if notification != "" {
		// Another tenant's notification is not found, like an unknown one.
		if s.notificationLocked(tenant, notification) == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "notification not found"})
			return
		}
	} else {
		linked = s.latestAboutTaskLocked(tenant, user, task)
	}
	s.feedback = append(s.feedback, body)
	s.signals = append(s.signals, signal{tenant: tenant, user: user, kind: kind, notification: linked, at: s.now()})
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "recorded"})
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	if err := CheckRule(body); err != nil {
		badRequest(w, err)
		return
	}
	s.mu.Lock()
	id := s.nextIDLocked("rule")
	s.rules = append(s.rules, storedRule{id: id, body: body})
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenantId")
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	s.mu.Lock()
	out := []map[string]any{}
	for _, rule := range s.rules {
		if rule.body["tenantId"] != tenant {
			continue
		}
		m := map[string]any{"id": rule.id}
		for k, v := range rule.body {
			m[k] = v
		}
		out = append(out, m)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (s *Server) handleListSuggestions(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenantId")
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	out := []Suggestion{}
	for _, sg := range s.Suggestions() {
		if t, _ := sg.Body["tenantId"].(string); t == tenant {
			out = append(out, sg)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
}

func (s *Server) handleCreateSuggestion(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	if err := CheckSuggestion(body); err != nil {
		badRequest(w, err)
		return
	}
	s.mu.Lock()
	sg := *s.addSuggestionLocked(body)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, sg)
}

// Learning limits (R6): AgentPulse bounds learned adjustments to plus or
// minus 20, and a suggestion rests on at least 5 signals.
const (
	MaxAdjustment  = 20
	MinSignalCount = 5
)

// CheckSuggestion returns why AgentPulse would refuse a submitted learning
// suggestion (R6), or nil: a valid tenant; a ruleId or an eventPattern
// {eventType, label}, label a string or null; currentAdjustment and
// proposedAdjustment numbers within -20 and 20; a signalCount of at least
// 5; and a rationale.
func CheckSuggestion(body map[string]any) error {
	tenant, _ := body["tenantId"].(string)
	if err := CheckTenant(tenant); err != nil {
		return err
	}
	rule, _ := body["ruleId"].(string)
	pattern, hasPattern := body["eventPattern"].(map[string]any)
	switch {
	case rule == "" && !hasPattern:
		return fmt.Errorf("ruleId or eventPattern is required")
	case hasPattern:
		if eventType, _ := pattern["eventType"].(string); eventType == "" {
			return fmt.Errorf("eventPattern.eventType is required")
		}
		switch pattern["label"].(type) {
		case nil, string:
		default:
			return fmt.Errorf("eventPattern.label must be a string or null")
		}
	}
	for _, field := range []string{"currentAdjustment", "proposedAdjustment"} {
		if adj, isNum := body[field].(float64); !isNum || adj < -MaxAdjustment || adj > MaxAdjustment {
			return fmt.Errorf("%s must be a number within -%d and %d", field, MaxAdjustment, MaxAdjustment)
		}
	}
	if n, isNum := body["signalCount"].(float64); !isNum || n < MinSignalCount {
		return fmt.Errorf("signalCount must be a number of at least %d", MinSignalCount)
	}
	if rationale, _ := body["rationale"].(string); rationale == "" {
		return fmt.Errorf("rationale is required")
	}
	return nil
}

// feedbackGroup is one row of the feedback analytics (R7).
type feedbackGroup struct {
	EventType     string  `json:"eventType"`
	Label         *string `json:"label"`
	Delivered     int     `json:"delivered"`
	Acted         int     `json:"acted"`
	Dismissed     int     `json:"dismissed"`
	Muted         int     `json:"muted"`
	Snoozed       int     `json:"snoozed"`
	DistinctUsers int     `json:"distinctUsers"`
	users         map[string]bool
}

// handleAnalytics serves R7: the tenant's notifications accepted since the
// given time, and the feedback signals received since then, counted per
// event type and per payload label. A notification counts once under each of
// its labels, or under a null label when it has none. A signal counts under
// the notification it names, or, without one, the user's latest notification
// about its task (AP-12); a signal linked to no notification is not counted.
// distinctUsers is how many users the group's counted signals came from.
// Groups are ordered by event type, then label, null first.
func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tenant := q.Get("tenantId")
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	since, err := time.Parse(time.RFC3339, q.Get("since"))
	if err != nil {
		badRequest(w, fmt.Errorf("since must be an RFC 3339 time"))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	type key struct {
		eventType, label string
		labelled         bool
	}
	groups := map[key]*feedbackGroup{}
	keysOf := func(n Notification) []key {
		eventType, _ := n.Body["eventType"].(string)
		payload, _ := n.Body["payload"].(map[string]any)
		labels, _ := payload["labels"].([]any)
		var out []key
		for _, l := range labels {
			if label, ok := l.(string); ok && !slices.Contains(out, key{eventType, label, true}) {
				out = append(out, key{eventType, label, true})
			}
		}
		if len(out) == 0 {
			out = []key{{eventType: eventType}}
		}
		return out
	}
	group := func(k key) *feedbackGroup {
		g := groups[k]
		if g == nil {
			g = &feedbackGroup{EventType: k.eventType, users: map[string]bool{}}
			if k.labelled {
				label := k.label
				g.Label = &label
			}
			groups[k] = g
		}
		return g
	}
	for _, n := range s.notifications {
		if t, _ := n.Body["tenantId"].(string); t != tenant || n.At.Before(since) {
			continue
		}
		for _, k := range keysOf(n) {
			group(k).Delivered++
		}
	}
	for _, sg := range s.signals {
		if sg.tenant != tenant || sg.notification == "" || sg.at.Before(since) {
			continue
		}
		n := s.notificationLocked(tenant, sg.notification)
		if n == nil {
			continue
		}
		for _, k := range keysOf(*n) {
			g := group(k)
			switch sg.kind {
			case "acted":
				g.Acted++
			case "dismissed":
				g.Dismissed++
			case "muted":
				g.Muted++
			case "snoozed":
				g.Snoozed++
			}
			g.users[sg.user] = true
		}
	}
	out := make([]*feedbackGroup, 0, len(groups))
	for _, g := range groups {
		g.DistinctUsers = len(g.users)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.EventType != b.EventType {
			return a.EventType < b.EventType
		}
		if (a.Label == nil) != (b.Label == nil) {
			return a.Label == nil
		}
		return a.Label != nil && *a.Label < *b.Label
	})
	writeJSON(w, http.StatusOK, map[string]any{"since": since.UTC().Format(time.RFC3339), "groups": out})
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	var status string
	switch body["decision"] {
	case "approve":
		status = "APPROVED"
	case "reject":
		status = "REJECTED"
	default:
		badRequest(w, fmt.Errorf("decision must be approve or reject"))
		return
	}
	// AP-12: a call naming an object by ID names its tenant, and another
	// tenant's suggestion is not found, like an unknown one.
	tenant, _ := body["tenantId"].(string)
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	id := r.PathValue("id")

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sg := range s.suggestions {
		if t, _ := sg.Body["tenantId"].(string); sg.ID != id || t != tenant {
			continue
		}
		if sg.Status != "PENDING_APPROVAL" {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "suggestion already decided"})
			return
		}
		sg.Status = status
		writeJSON(w, http.StatusOK, *sg)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "suggestion not found"})
}

// Feed page sizes.
const (
	defaultFeedLimit = 20
	maxFeedLimit     = 100
)

// handleFeed serves a user's feed newest first. The cursor is the position
// of the next item, opaque to the caller.
func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenantId")
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	limit, start := defaultFeedLimit, 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxFeedLimit {
			badRequest(w, fmt.Errorf("limit must be 1 to %d", maxFeedLimit))
			return
		}
		limit = n
	}
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badRequest(w, fmt.Errorf("cursor is invalid"))
			return
		}
		start = n
	}
	s.mu.Lock()
	all := s.feeds[tenant+"/"+r.PathValue("userId")]
	s.mu.Unlock()
	start = min(start, len(all))
	end := min(start+limit, len(all))
	var next any
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": append([]FeedItem{}, all[start:end]...), "nextCursor": next})
}

// --- helpers ---------------------------------------------------------------

// notificationLocked returns the tenant's notification with that ID, or nil.
func (s *Server) notificationLocked(tenant, id string) *Notification {
	for i := range s.notifications {
		n := &s.notifications[i]
		if t, _ := n.Body["tenantId"].(string); n.ID == id && t == tenant {
			return n
		}
	}
	return nil
}

// latestAboutTaskLocked returns the ID of the user's latest notification in
// the tenant whose payload.taskId is task, or "" when there is none.
func (s *Server) latestAboutTaskLocked(tenant, user, task string) string {
	for i := len(s.notifications) - 1; i >= 0; i-- {
		n := s.notifications[i]
		t, _ := n.Body["tenantId"].(string)
		u, _ := n.Body["userId"].(string)
		payload, _ := n.Body["payload"].(map[string]any)
		if id, _ := payload["taskId"].(string); t == tenant && u == user && task != "" && id == task {
			return n.ID
		}
	}
	return ""
}

func (s *Server) nextIDLocked(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s_%06d", prefix, s.seq)
}

func (s *Server) addSuggestionLocked(body map[string]any) *Suggestion {
	sg := &Suggestion{ID: s.nextIDLocked("sug"), Status: "PENDING_APPROVAL", Body: body}
	s.suggestions = append(s.suggestions, sg)
	return sg
}

func decodeObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		badRequest(w, fmt.Errorf("body must be a JSON object"))
		return nil, false
	}
	return body, true
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
