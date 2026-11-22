package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rtrtarun/taskify/internal/domain"
)

// LearningSubmitter is the part of service.LearningService
// ProposeNotificationRule uses.
type LearningSubmitter interface {
	Submit(ctx context.Context, s domain.LearningSuggestion) (bool, error)
}

// PatternSource is the facts of a learning run (Scope.Facts): the dismissal
// patterns Go code found, by the ID the model was shown.
type PatternSource interface {
	LearningPattern(id string) (domain.LearningPattern, bool)
}

// ProposeNotificationRuleName is the tool's name (section 5.7).
const ProposeNotificationRuleName = "ProposeNotificationRule"

// ProposeNotificationRule turns one of the run's dismissal patterns into a
// learning suggestion for AgentPulse (M11-03): a new score adjustment for
// notifications of the pattern's event type and label. The model chooses
// the adjustment, within -20 and 20, and writes the rationale; the scope and
// the evidence (the signal count and the current adjustment) come from the
// pattern Go code found, never from the model, and the tenant from the job.
//
// Its tier is 1 and it is a Suggester: executing it records the suggestion
// and queues it for AgentPulse, which holds it as PENDING_APPROVAL. That is
// the Tier 2 decision of section 5.7: nothing takes effect until an admin
// approves it through the learning-suggestion endpoints. A job submits each
// pattern at most once.
type ProposeNotificationRule struct {
	Suggestions LearningSubmitter
}

var (
	_ Checker   = ProposeNotificationRule{}
	_ Suggester = ProposeNotificationRule{}
)

// ProposeNotificationRuleArgs are the tool's arguments.
type ProposeNotificationRuleArgs struct {
	PatternID          string `json:"pattern_id"`
	ProposedAdjustment int    `json:"proposed_adjustment"`
	Rationale          string `json:"rationale"`
}

// SuggestionResult is the tool's output.
type SuggestionResult struct {
	// Status is "submitted", or "already_submitted" when the job had
	// submitted the pattern before.
	Status             string  `json:"status"`
	EventType          string  `json:"event_type"`
	Label              *string `json:"label"`
	ProposedAdjustment int     `json:"proposed_adjustment"`
	Message            string  `json:"message"`
}

func (ProposeNotificationRule) Name() string { return ProposeNotificationRuleName }

func (ProposeNotificationRule) Description() string {
	return "Propose a new notification score adjustment for one dismissal pattern from the facts. " +
		"AgentPulse holds the proposal until an admin approves it. Call it at most once per pattern."
}

func (ProposeNotificationRule) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern_id": {"type": "string", "minLength": 1, "maxLength": 16,
				"description": "The pattern_id of one pattern in the facts."},
			"proposed_adjustment": {"type": "integer", "minimum": -20, "maximum": 20,
				"description": "The new score adjustment for notifications matching the pattern, from -20 to 20. Negative lowers their score, so they interrupt less; it must differ from the pattern's current_adjustment."},
			"rationale": {"type": "string", "minLength": 1, "maxLength": 1000,
				"description": "Why, in one or two sentences an admin can check against the numbers."}
		},
		"required": ["pattern_id", "proposed_adjustment", "rationale"],
		"additionalProperties": false
	}`)
}

func (ProposeNotificationRule) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier1 }

// DecidedUpstream: an admin decides the suggestion at AgentPulse.
func (ProposeNotificationRule) DecidedUpstream() bool { return true }

// Check refuses a pattern the run was not given and an adjustment equal to
// the current one, before anything is recorded. The schema has already
// refused an adjustment outside -20 and 20.
func (ProposeNotificationRule) Check(s Scope, args json.RawMessage) error {
	_, _, err := proposal(s, args)
	return err
}

// Execute submits the suggestion.
func (t ProposeNotificationRule) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	a, p, err := proposal(s, args)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err.Error())
	}
	created, err := t.Suggestions.Submit(ctx, domain.LearningSuggestion{
		TenantID:           s.TenantID,
		JobID:              s.JobID,
		Pattern:            p.Pattern(),
		CurrentAdjustment:  p.CurrentAdjustment,
		ProposedAdjustment: a.ProposedAdjustment,
		SignalCount:        p.SignalCount,
		Rationale:          a.Rationale,
	})
	if err != nil {
		return nil, err
	}
	out := SuggestionResult{Status: "submitted", EventType: p.EventType, Label: p.Label, ProposedAdjustment: a.ProposedAdjustment,
		Message: "Submitted to AgentPulse, which holds it until an admin approves or rejects it. Nothing changes before then."}
	if !created {
		out.Status, out.Message = "already_submitted", "This job already submitted a suggestion for the pattern."
	}
	return out, nil
}

// proposal decodes the arguments and finds their pattern in the run's facts.
func proposal(s Scope, args json.RawMessage) (ProposeNotificationRuleArgs, domain.LearningPattern, error) {
	var a ProposeNotificationRuleArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, domain.LearningPattern{}, err
	}
	facts, ok := s.Facts.(PatternSource)
	if !ok {
		return a, domain.LearningPattern{}, errors.New("this run has no dismissal patterns to propose on")
	}
	p, ok := facts.LearningPattern(a.PatternID)
	if !ok {
		return a, domain.LearningPattern{}, fmt.Errorf("pattern_id %q is not one of the patterns in the facts", a.PatternID)
	}
	if a.ProposedAdjustment == p.CurrentAdjustment {
		return a, domain.LearningPattern{}, fmt.Errorf("proposed_adjustment %d is the pattern's current adjustment; propose a change or skip the pattern",
			a.ProposedAdjustment)
	}
	return a, p, nil
}
