package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// fakeSubmitter records submissions; a job and pattern seen before is not
// new, as LearningService reports.
type fakeSubmitter struct {
	mu   sync.Mutex
	subs []domain.LearningSuggestion
}

func (f *fakeSubmitter) Submit(_ context.Context, s domain.LearningSuggestion) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := s.Validate(); err != nil {
		return false, err
	}
	for _, have := range f.subs {
		if have.JobID == s.JobID && have.Pattern.Equal(s.Pattern) {
			return false, nil
		}
	}
	f.subs = append(f.subs, s)
	return true, nil
}

// patterns is a run's facts.
type patterns []domain.LearningPattern

func (ps patterns) LearningPattern(id string) (domain.LearningPattern, bool) {
	for _, p := range ps {
		if p.ID == id {
			return p, true
		}
	}
	return domain.LearningPattern{}, false
}

func proposeCall(id string, args map[string]any) llm.ToolCall {
	raw, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: ProposeNotificationRuleName, Arguments: string(raw)}
}

// TestProposeNotificationRule is M11-03's tool: a call names one of the
// run's patterns and an adjustment within -20 and 20; the scope and the
// evidence come from the pattern and the job, never from the model. It runs
// even when the workspace requires approval for everything, because what it
// submits waits at AgentPulse for an admin's decision.
func TestProposeNotificationRule(t *testing.T) {
	b := repotest.Memory()
	reg := NewRegistry(b.Tasks)
	sub := &fakeSubmitter{}
	if err := reg.Register(ProposeNotificationRule{Suggestions: sub}); err != nil {
		t.Fatal(err)
	}
	docs := "docs"
	facts := patterns{
		{ID: "p1", EventType: "task.comment_added", Label: &docs, Delivered: 20, Dismissed: 14, Muted: 2, DistinctUsers: 4, SignalCount: 16},
		{ID: "p2", EventType: "task.updated", Delivered: 10, Dismissed: 6, DistinctUsers: 2, SignalCount: 6, CurrentAdjustment: -5},
	}
	job := uuid.New()
	scope := Scope{TenantID: "tenant_acme", JobID: job, AgentType: domain.AgentLearning, RequireHITL: true,
		AllowedTools: []string{ProposeNotificationRuleName}, Facts: facts}

	// Refused before anything is submitted: out of range, no such pattern,
	// no change, missing fields, a model-supplied scope.
	for name, args := range map[string]map[string]any{
		"adjustment -21":    {"pattern_id": "p1", "proposed_adjustment": -21, "rationale": "r"},
		"adjustment 21":     {"pattern_id": "p1", "proposed_adjustment": 21, "rationale": "r"},
		"not an integer":    {"pattern_id": "p1", "proposed_adjustment": -7.5, "rationale": "r"},
		"unknown pattern":   {"pattern_id": "p9", "proposed_adjustment": -10, "rationale": "r"},
		"no change":         {"pattern_id": "p2", "proposed_adjustment": -5, "rationale": "r"},
		"no rationale":      {"pattern_id": "p1", "proposed_adjustment": -10},
		"model names label": {"pattern_id": "p1", "proposed_adjustment": -10, "rationale": "r", "label": "ops"},
	} {
		_, err := reg.Dispatch(ctx, scope, proposeCall("c1", args))
		var ce *CallError
		if !errors.As(err, &ce) || ce.Code != CodeInvalidArguments {
			t.Errorf("%s: %v", name, err)
		}
	}
	noFacts := scope
	noFacts.Facts = nil
	if _, err := reg.Dispatch(ctx, noFacts, proposeCall("c1", map[string]any{"pattern_id": "p1", "proposed_adjustment": -10, "rationale": "r"})); !errors.Is(err, ErrRejected) {
		t.Errorf("a run without patterns: %v", err)
	}
	if len(sub.subs) != 0 {
		t.Fatalf("a refused call submitted %v", sub.subs)
	}

	// A valid call runs at once, under require_hitl_for_all, and submits
	// the pattern's scope and evidence with the model's adjustment.
	res, err := reg.Dispatch(ctx, scope, proposeCall("c2", map[string]any{"pattern_id": "p1", "proposed_adjustment": -12,
		"rationale": "16 of 20 comments on docs tasks were dismissed or muted."}))
	if err != nil || !res.Executed() || res.Tier != domain.Tier1 {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	if out := res.Output.(SuggestionResult); out.Status != "submitted" || out.EventType != "task.comment_added" || *out.Label != "docs" {
		t.Errorf("output = %+v", out)
	}
	if len(sub.subs) != 1 {
		t.Fatalf("submissions = %v", sub.subs)
	}
	s := sub.subs[0]
	if s.TenantID != "tenant_acme" || s.JobID != job || !s.Pattern.Equal(facts[0].Pattern()) || s.SignalCount != 16 ||
		s.CurrentAdjustment != 0 || s.ProposedAdjustment != -12 || s.Rationale == "" {
		t.Errorf("submitted = %+v", s)
	}
	// The unlabelled pattern carries its current adjustment.
	if _, err := reg.Dispatch(ctx, scope, proposeCall("c3", map[string]any{"pattern_id": "p2", "proposed_adjustment": -10, "rationale": "r"})); err != nil {
		t.Fatal(err)
	}
	if s := sub.subs[1]; s.Pattern.Label != nil || s.CurrentAdjustment != -5 || s.SignalCount != 6 {
		t.Errorf("second = %+v", s)
	}
	// The same pattern again in the job submits nothing more.
	res, err = reg.Dispatch(ctx, scope, proposeCall("c4", map[string]any{"pattern_id": "p1", "proposed_adjustment": -15, "rationale": "r"}))
	if err != nil || res.Output.(SuggestionResult).Status != "already_submitted" || len(sub.subs) != 2 {
		t.Errorf("repeat = %+v, %v, %d submissions", res, err, len(sub.subs))
	}

	// Execute alone, as when applying a stored proposal, has no patterns
	// and refuses.
	if _, err := (ProposeNotificationRule{Suggestions: sub}).Execute(ctx, Scope{TenantID: "tenant_acme", JobID: job},
		json.RawMessage(`{"pattern_id":"p1","proposed_adjustment":-10,"rationale":"r"}`)); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Execute without facts: %v", err)
	}
}

// TestRequireHITLStillHoldsOtherTools: only a Suggester bypasses
// require_hitl_for_all; any other Tier 1 tool is still a proposal.
func TestRequireHITLStillHoldsOtherTools(t *testing.T) {
	reg := NewRegistry(repotest.Memory().Tasks)
	plain := &fakeTool{name: "Plain", params: `{"type":"object","properties":{},"additionalProperties":false}`}
	if err := reg.Register(plain); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Dispatch(ctx, Scope{TenantID: "tenant_acme", RequireHITL: true}, llm.ToolCall{ID: "c", Name: "Plain", Arguments: "{}"})
	if err != nil || res.Executed() || plain.ran() != 0 {
		t.Errorf("a plain Tier 1 tool under require_hitl_for_all: %+v, %v", res, err)
	}
}
