package domain

import (
	"time"
)

// The learning agent (feature A5, M11) turns dismissal patterns in
// AgentPulse's feedback analytics into learning suggestions, which AgentPulse
// holds as PENDING_APPROVAL until an admin decides (decision 6).

// AgentPulse's thresholds for a learning suggestion (R6, M11): at least 5
// signals from at least 2 users, within a 30-day window.
const (
	LearningMinSignals = 5
	LearningMinUsers   = 2
	LearningWindow     = 30 * 24 * time.Hour
)

// Taskify's own limits on what the learning agent proposes.
const (
	// LearningMinNegativeRatio is the share of a pattern's deliveries that
	// must have been dismissed or muted for the pattern to count as
	// consistent: half.
	LearningMinNegativeRatio = 0.5
	// LearningMaxPatterns is how many patterns one run is given, those with
	// the most signals first.
	LearningMaxPatterns = 5
	// MaxLearningAdjustment bounds a proposed adjustment: AgentPulse accepts
	// -20 to 20.
	MaxLearningAdjustment = 20
)

// EventPattern names the scope a learning suggestion adjusts: one event type
// and one payload label. A nil Label is the notifications of that event type
// that carry no label. Its JSON is AgentPulse's eventPattern.
type EventPattern struct {
	EventType string  `json:"eventType"`
	Label     *string `json:"label"`
}

// Equal reports whether two patterns name the same scope.
func (p EventPattern) Equal(o EventPattern) bool {
	if p.EventType != o.EventType || (p.Label == nil) != (o.Label == nil) {
		return false
	}
	return p.Label == nil || *p.Label == *o.Label
}

// String is the pattern for logs and messages: "task.updated, label docs".
func (p EventPattern) String() string {
	if p.Label == nil {
		return p.EventType + ", no label"
	}
	return p.EventType + ", label " + *p.Label
}

// FeedbackGroup is AgentPulse's count of notifications and feedback signals
// for one event type and label over a window (R7). DistinctUsers is how many
// users the group's signals came from.
type FeedbackGroup struct {
	EventType     string
	Label         *string
	Delivered     int
	Acted         int
	Dismissed     int
	Muted         int
	Snoozed       int
	DistinctUsers int
}

// Pattern is the group's scope.
func (g FeedbackGroup) Pattern() EventPattern {
	return EventPattern{EventType: g.EventType, Label: g.Label}
}

// FeedbackAnalytics is a tenant's feedback counts since a time (R7).
type FeedbackAnalytics struct {
	Since  time.Time
	Groups []FeedbackGroup
}

// LearningPattern is a dismissal pattern that meets the thresholds: what the
// learning agent shows the model and builds a suggestion from. Its JSON,
// snake_case, is what the model reads and the job's report keeps.
type LearningPattern struct {
	// ID names the pattern within one run, such as "p1".
	ID        string  `json:"pattern_id"`
	EventType string  `json:"event_type"`
	Label     *string `json:"label"`
	Delivered int     `json:"delivered"`
	Acted     int     `json:"acted"`
	Dismissed int     `json:"dismissed"`
	Muted     int     `json:"muted"`
	Snoozed   int     `json:"snoozed"`
	// DistinctUsers is how many users the signals came from.
	DistinctUsers int `json:"distinct_users"`
	// SignalCount is the signals the suggestion rests on: dismissed plus
	// muted.
	SignalCount int `json:"signal_count"`
	// DismissRatio and MuteRatio are shares of the deliveries, rounded to
	// three places.
	DismissRatio float64 `json:"dismiss_ratio"`
	MuteRatio    float64 `json:"mute_ratio"`
	// CurrentAdjustment is the adjustment in force for the scope, as far as
	// Taskify can tell: the latest approved suggestion's, or 0.
	CurrentAdjustment int `json:"current_adjustment"`
}

// Pattern is the scope the pattern was found in.
func (p LearningPattern) Pattern() EventPattern {
	return EventPattern{EventType: p.EventType, Label: p.Label}
}
