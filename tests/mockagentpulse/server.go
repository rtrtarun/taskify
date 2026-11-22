// Package mockagentpulse is a stand-in for the AgentPulse notification-service.
//
// It implements the /v1/ API that AgentPulse builds in its milestone 40, as
// agreed in docs/blockers.md ("Agreed with AgentPulse"), records every request
// so tests can assert on them, and can be scripted to fail. It enforces the
// validation AgentPulse applies from slice 40b: tenant IDs, the fixed
// actionUrls keys and the IDs each needs, and critical rule conditions. Any
// later change to the contract changes this mock in the same commit.
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
	rules         []storedRule
	suggestions   []*Suggestion
	feeds         map[string][]FeedItem // key: tenant + "/" + user
	delays        map[string]time.Duration

	mux *http.ServeMux
}

// New returns a ready mock.
func New() *Server {
	s := &Server{
		failures: map[string]*failure{},
		byKey:    map[string]string{},
		feeds:    map[string][]FeedItem{},
		delays:   map[string]time.Duration{},
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
	s.mux.HandleFunc("GET /v1/tenants/{tenantId}/users/{userId}/in-app-feed", s.handleFeed)
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
	s.rules = nil
	s.suggestions = nil
	s.feeds = map[string][]FeedItem{}
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

// --- handlers --------------------------------------------------------------

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
	s.notifications = append(s.notifications, Notification{ID: id, Body: body})
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
	if user, _ := body["userId"].(string); user == "" {
		badRequest(w, fmt.Errorf("userId is required"))
		return
	}
	switch body["signal"] {
	case "acted", "dismissed", "snoozed", "muted":
	default:
		badRequest(w, fmt.Errorf("signal must be acted, dismissed, snoozed, or muted"))
		return
	}
	s.mu.Lock()
	s.feedback = append(s.feedback, body)
	s.mu.Unlock()
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
	tenant, _ := body["tenantId"].(string)
	if err := CheckTenant(tenant); err != nil {
		badRequest(w, err)
		return
	}
	// AgentPulse bounds learned adjustments to plus or minus 20.
	if adj, isNum := body["proposedAdjustment"].(float64); !isNum || adj < -20 || adj > 20 {
		badRequest(w, fmt.Errorf("proposedAdjustment must be a number within -20 and 20"))
		return
	}
	s.mu.Lock()
	sg := *s.addSuggestionLocked(body)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, sg)
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
	id := r.PathValue("id")

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sg := range s.suggestions {
		if sg.ID != id {
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
