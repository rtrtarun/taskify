package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEventPattern(t *testing.T) {
	docs, docs2, ops := "docs", "docs", "ops"
	for _, tc := range []struct {
		a, b  EventPattern
		equal bool
	}{
		{EventPattern{"task.updated", &docs}, EventPattern{"task.updated", &docs2}, true},
		{EventPattern{"task.updated", nil}, EventPattern{"task.updated", nil}, true},
		{EventPattern{"task.updated", &docs}, EventPattern{"task.updated", &ops}, false},
		{EventPattern{"task.updated", &docs}, EventPattern{"task.updated", nil}, false},
		{EventPattern{"task.updated", nil}, EventPattern{"task.created", nil}, false},
	} {
		if tc.a.Equal(tc.b) != tc.equal || tc.b.Equal(tc.a) != tc.equal {
			t.Errorf("%s = %s: %v, want %v", tc.a, tc.b, tc.a.Equal(tc.b), tc.equal)
		}
	}
	// AgentPulse's JSON: label null, never left out, for no label.
	raw, _ := json.Marshal(EventPattern{EventType: "task.updated"})
	if string(raw) != `{"eventType":"task.updated","label":null}` {
		t.Errorf("JSON = %s", raw)
	}
	if s := (EventPattern{"task.updated", &docs}).String(); s != "task.updated, label docs" {
		t.Errorf("String = %q", s)
	}
}

func validSuggestion() LearningSuggestion {
	docs := "docs"
	return LearningSuggestion{ID: uuid.New(), TenantID: "tenant_acme", JobID: uuid.New(),
		Pattern:           EventPattern{EventType: "task.comment_added", Label: &docs},
		CurrentAdjustment: -5, ProposedAdjustment: -12, SignalCount: 16, Rationale: "16 of 20 dismissed or muted."}
}

func TestLearningSuggestionValidate(t *testing.T) {
	if err := validSuggestion().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*LearningSuggestion){
		"bad tenant":           func(s *LearningSuggestion) { s.TenantID = "default" },
		"no job":               func(s *LearningSuggestion) { s.JobID = uuid.Nil },
		"no event type":        func(s *LearningSuggestion) { s.Pattern.EventType = "" },
		"adjustment over 20":   func(s *LearningSuggestion) { s.ProposedAdjustment = 21 },
		"adjustment under -20": func(s *LearningSuggestion) { s.ProposedAdjustment = -21 },
		"current out of range": func(s *LearningSuggestion) { s.CurrentAdjustment = -25 },
		"no change":            func(s *LearningSuggestion) { s.ProposedAdjustment = s.CurrentAdjustment },
		"four signals":         func(s *LearningSuggestion) { s.SignalCount = 4 },
		"no rationale":         func(s *LearningSuggestion) { s.Rationale = "  " },
		"long rationale":       func(s *LearningSuggestion) { s.Rationale = strings.Repeat("r", 1001) },
	} {
		s := validSuggestion()
		change(&s)
		if err := s.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, adj := range []int{-20, 20} {
		s := validSuggestion()
		s.ProposedAdjustment = adj
		if err := s.Validate(); err != nil {
			t.Errorf("adjustment %d: %v", adj, err)
		}
	}
}

// TestLearningSuggestionEntry: the outbox row carries AgentPulse's exact
// body, is keyed by job and pattern, and belongs to the job.
func TestLearningSuggestionEntry(t *testing.T) {
	s := validSuggestion()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	e, err := NewLearningSuggestionEntry(s, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tenantId":"tenant_acme","eventPattern":{"eventType":"task.comment_added","label":"docs"},` +
		`"currentAdjustment":-5,"proposedAdjustment":-12,"signalCount":16,"rationale":"16 of 20 dismissed or muted."}`
	if string(e.Payload) != want {
		t.Errorf("payload:\n got %s\nwant %s", e.Payload, want)
	}
	if e.EventType != EventLearningSuggestion || e.AggregateID != s.JobID || e.TenantID != "tenant_acme" || e.MaxAttempts != 7 ||
		e.Status != OutboxPending || !e.NextAttemptAt.Equal(now) {
		t.Errorf("entry = %+v", e)
	}
	if !strings.HasPrefix(e.IdempotencyKey, "taskify:tenant_acme:"+s.JobID.String()+":learning.suggestion:") || len(e.IdempotencyKey) > 320 {
		t.Errorf("key = %s", e.IdempotencyKey)
	}
	// The key tells patterns apart, labelled or not, and is the same for
	// the same job and pattern.
	unlabelled := s
	unlabelled.Pattern.Label = nil
	other := s
	ops := "ops"
	other.Pattern.Label = &ops
	keys := map[string]bool{}
	for _, x := range []LearningSuggestion{s, unlabelled, other} {
		keys[LearningSuggestionKey(x.TenantID, x.JobID, x.Pattern)] = true
	}
	if len(keys) != 3 || LearningSuggestionKey(s.TenantID, s.JobID, s.Pattern) != e.IdempotencyKey {
		t.Errorf("keys = %v", keys)
	}
	if e, err := NewLearningSuggestionEntry(unlabelled, 7, now); err != nil || !strings.Contains(string(e.Payload), `"label":null`) {
		t.Errorf("unlabelled payload = %s, %v", e.Payload, err)
	}
	s.ProposedAdjustment = 30
	if _, err := NewLearningSuggestionEntry(s, 7, now); !errors.Is(err, ErrValidation) {
		t.Errorf("out of range: %v", err)
	}
}

// TestLearningJob: one job per tenant per ISO week, naming its workspace.
func TestLearningJob(t *testing.T) {
	for at, week := range map[time.Time]string{
		time.Date(2026, 11, 22, 23, 59, 0, 0, time.UTC):                  "2026-W47", // a Sunday
		time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC):                    "2026-W48", // the Monday after
		time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC):                     "2026-W53",
		time.Date(2026, 11, 23, 1, 0, 0, 0, time.FixedZone("x", 2*3600)): "2026-W47", // still Sunday in UTC
	} {
		if got := LearningWeek(at); got != week {
			t.Errorf("LearningWeek(%s) = %s, want %s", at, got, week)
		}
	}
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	job, err := NewLearningJob(LearningTenant{TenantID: "tenant_acme", WorkspaceID: "ws_core"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if job.AgentType != AgentLearning || job.TaskID != nil || job.DedupKey != "learning:tenant_acme:week:2026-W48" || job.Status != JobPending {
		t.Errorf("job = %+v", job)
	}
	in, err := ParseLearningInput(job.InputContext)
	if err != nil || in != (LearningInput{Trigger: TriggerWeekly, WorkspaceID: "ws_core", Week: "2026-W48"}) {
		t.Errorf("input = %+v, %v", in, err)
	}
	if _, err := NewLearningJob(LearningTenant{TenantID: "tenant_acme"}, now); !errors.Is(err, ErrValidation) {
		t.Errorf("no workspace: %v", err)
	}
	if _, err := NewLearningJob(LearningTenant{TenantID: "default", WorkspaceID: "ws"}, now); !errors.Is(err, ErrValidation) {
		t.Errorf("bad tenant: %v", err)
	}
	for _, raw := range []string{`{}`, `{"trigger":"weekly","workspace_id":"ws"}`, `{"trigger":"stale","workspace_id":"ws","week":"2026-W48"}`, `[`} {
		if _, err := ParseLearningInput(json.RawMessage(raw)); !errors.Is(err, ErrValidation) {
			t.Errorf("input %s: %v", raw, err)
		}
	}
}
