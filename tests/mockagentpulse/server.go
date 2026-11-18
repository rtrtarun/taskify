// Package mockagentpulse is a stand-in for the AgentPulse notification-service.
//
// It implements the calls listed in docs/architecture.md section 5.5, records
// every request so tests can assert on them, and can be scripted to fail.
//
// It must not import any Taskify server package: the conformance suite reuses
// it as a black box.
//
// The shapes here follow the architecture document, not the AgentPulse source.
// Task M0-07 checks them against the real contract.
package mockagentpulse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Notification is an accepted /notify call, after deduplication.
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

// Server is the mock. The zero value is not usable; call New.
type Server struct {
	mu            sync.Mutex
	seq           int
	requests      []Request
	failures      map[string]*failure // key: "METHOD /path"
	notifications []Notification
	byKey         map[string]string // idempotencyKey -> notification ID
	duplicates    int
	feedback      []map[string]any
	rules         []map[string]any
	suggestions   []*Suggestion
	feeds         map[string][]FeedItem // key: tenant + "/" + user

	mux *http.ServeMux
}

// New returns a ready mock.
func New() *Server {
	s := &Server{
		failures: map[string]*failure{},
		byKey:    map[string]string{},
		feeds:    map[string][]FeedItem{},
		mux:      http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	s.mux.HandleFunc("POST /notify", s.handleNotify)
	s.mux.HandleFunc("POST /feedback", s.handleFeedback)
	s.mux.HandleFunc("POST /rules", s.handleCreateRule)
	s.mux.HandleFunc("GET /rules", s.handleListRules)
	s.mux.HandleFunc("GET /learning/suggestions", s.handleListSuggestions)
	s.mux.HandleFunc("POST /learning/suggestions", s.handleCreateSuggestion)
	s.mux.HandleFunc("POST /learning/suggestions/{id}/decision", s.handleDecision)
	s.mux.HandleFunc("GET /internal/v1/tenants/{tenantId}/users/{userId}/in-app-feed", s.handleFeed)
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
	s.mu.Unlock()

	if status != 0 {
		writeJSON(w, status, map[string]any{"error": "scripted failure"})
		return
	}
	s.mux.ServeHTTP(w, r)
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

// SeedFeed sets the in-app feed returned for a tenant and user.
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

// Notifications returns accepted notifications: one per distinct
// idempotencyKey, in arrival order.
func (s *Server) Notifications() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notification(nil), s.notifications...)
}

// Duplicates returns how many /notify calls were acknowledged as duplicates.
func (s *Server) Duplicates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicates
}

// Feedback returns the bodies of accepted /feedback calls.
func (s *Server) Feedback() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.feedback...)
}

// Rules returns the bodies of accepted /rules calls.
func (s *Server) Rules() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.rules...)
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
	for _, field := range []string{"idempotencyKey", "tenantId", "userId", "eventType", "title", "body"} {
		if v, _ := body[field].(string); v == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": field + " is required"})
			return
		}
	}
	key := body["idempotencyKey"].(string)

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
	switch body["signal"] {
	case "acted", "dismissed", "snoozed", "muted":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "signal must be acted, dismissed, snoozed, or muted"})
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
	s.mu.Lock()
	id := s.nextIDLocked("rule")
	s.rules = append(s.rules, body)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleListRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rules": s.Rules()})
}

func (s *Server) handleListSuggestions(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenantId")
	out := []Suggestion{}
	for _, sg := range s.Suggestions() {
		if t, _ := sg.Body["tenantId"].(string); tenant == "" || t == tenant {
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
	if v, _ := body["tenantId"].(string); v == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenantId is required"})
		return
	}
	// AgentPulse bounds learned adjustments to plus or minus 20.
	if adj, isNum := body["proposedAdjustment"].(float64); !isNum || adj < -20 || adj > 20 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "proposedAdjustment must be a number within -20 and 20"})
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "decision must be approve or reject"})
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

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("tenantId") + "/" + r.PathValue("userId")
	s.mu.Lock()
	items := append([]FeedItem{}, s.feeds[key]...)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"notifications": items})
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body must be a JSON object"})
		return nil, false
	}
	return body, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
