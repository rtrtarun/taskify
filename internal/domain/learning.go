package domain

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
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

// EventLearningSuggestion is the outbox event type that submits a learning
// suggestion to AgentPulse (M11-03). The relay sends it to
// POST /v1/learning/suggestions instead of /v1/notify.
const EventLearningSuggestion = "learning.suggestion"

// Limits on a suggestion's rationale, written by the model.
const maxRationaleLen = 1000

// LearningSuggestion is a suggestion the learning agent submits: a new
// score adjustment for one event pattern, with the evidence it rests on.
// AgentPulse holds it as PENDING_APPROVAL; nothing changes until an admin
// decides through the learning-suggestion endpoints (decision 6).
type LearningSuggestion struct {
	ID       uuid.UUID
	TenantID string
	// JobID is the learning job that submitted it. A job submits a pattern
	// at most once.
	JobID              uuid.UUID
	Pattern            EventPattern
	CurrentAdjustment  int
	ProposedAdjustment int
	SignalCount        int
	Rationale          string
	CreatedAt          time.Time
}

// Validate checks the suggestion against AgentPulse's limits: adjustments
// within -20 and 20, a change from the current one, at least 5 signals, and
// a rationale.
func (s LearningSuggestion) Validate() error {
	if err := ValidateTenantID(s.TenantID); err != nil {
		return err
	}
	bounded := func(n int) bool { return n >= -MaxLearningAdjustment && n <= MaxLearningAdjustment }
	switch {
	case s.JobID == uuid.Nil:
		return invalid("a learning suggestion needs its job")
	case s.Pattern.EventType == "":
		return invalid("a learning suggestion needs an event type")
	case !bounded(s.ProposedAdjustment):
		return invalid("proposed_adjustment must be between -%d and %d", MaxLearningAdjustment, MaxLearningAdjustment)
	case !bounded(s.CurrentAdjustment):
		return invalid("current_adjustment must be between -%d and %d", MaxLearningAdjustment, MaxLearningAdjustment)
	case s.ProposedAdjustment == s.CurrentAdjustment:
		return invalid("proposed_adjustment must differ from the current adjustment, %d", s.CurrentAdjustment)
	case s.SignalCount < LearningMinSignals:
		return invalid("a learning suggestion needs at least %d signals", LearningMinSignals)
	case strings.TrimSpace(s.Rationale) == "" || utf8.RuneCountInString(s.Rationale) > maxRationaleLen:
		return invalid("rationale must be 1 to %d characters", maxRationaleLen)
	}
	return nil
}

// LearningSuggestionBody is the payload of a learning.suggestion outbox row:
// the POST /v1/learning/suggestions body, camelCase as AgentPulse expects.
type LearningSuggestionBody struct {
	TenantID           string       `json:"tenantId"`
	EventPattern       EventPattern `json:"eventPattern"`
	CurrentAdjustment  int          `json:"currentAdjustment"`
	ProposedAdjustment int          `json:"proposedAdjustment"`
	SignalCount        int          `json:"signalCount"`
	Rationale          string       `json:"rationale"`
}

// LearningSuggestionKey is the outbox key of a job's suggestion for a
// pattern. It is Taskify's own: AgentPulse takes no key for suggestions. The
// pattern is hashed, since event types and labels come from AgentPulse.
func LearningSuggestionKey(tenantID string, jobID uuid.UUID, p EventPattern) string {
	h := sha256.New()
	h.Write([]byte(p.EventType))
	if p.Label != nil {
		h.Write([]byte{0})
		h.Write([]byte(*p.Label))
	}
	return fmt.Sprintf("%s:%s:%s:%s:%x", Source, tenantID, jobID, EventLearningSuggestion, h.Sum(nil)[:16])
}

// NewLearningSuggestionEntry returns the outbox row that submits s to
// AgentPulse. Its aggregate is the learning job.
func NewLearningSuggestionEntry(s LearningSuggestion, maxAttempts int, now time.Time) (OutboxEntry, error) {
	if err := s.Validate(); err != nil {
		return OutboxEntry{}, err
	}
	body, err := json.Marshal(LearningSuggestionBody{TenantID: s.TenantID, EventPattern: s.Pattern,
		CurrentAdjustment: s.CurrentAdjustment, ProposedAdjustment: s.ProposedAdjustment, SignalCount: s.SignalCount,
		Rationale: s.Rationale})
	if err != nil {
		return OutboxEntry{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return OutboxEntry{}, err
	}
	now = now.UTC()
	return OutboxEntry{
		ID:             id,
		TenantID:       s.TenantID,
		AggregateID:    s.JobID,
		EventType:      EventLearningSuggestion,
		IdempotencyKey: LearningSuggestionKey(s.TenantID, s.JobID, s.Pattern),
		Payload:        body,
		TraceContext:   map[string]string{},
		Status:         OutboxPending,
		MaxAttempts:    maxAttempts,
		NextAttemptAt:  now,
		CreatedAt:      now,
	}, nil
}

// TriggerWeekly is the trigger in the input context of a learning job.
const TriggerWeekly = "weekly"

// LearningWeek is the ISO week of t, in UTC, such as "2026-W47": the period
// a tenant has one learning job for (G16).
func LearningWeek(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// LearningJobDedupKey is the dedup key of a tenant's learning job: one per
// ISO week.
func LearningJobDedupKey(tenantID, week string) string {
	return AgentLearning + ":" + tenantID + ":week:" + week
}

// LearningTenant is a tenant the scheduler queues a learning job for: one
// with a workspace whose settings allow the learning agent.
type LearningTenant struct {
	TenantID string
	// WorkspaceID is the first such workspace by ID, whose settings the
	// runtime checks (D23).
	WorkspaceID string
}

// LearningInput is the input context of a learning job. The job has no
// task; WorkspaceID is the workspace whose settings govern it.
type LearningInput struct {
	Trigger     string `json:"trigger"`
	WorkspaceID string `json:"workspace_id"`
	// Week is the ISO week the job is for (LearningWeek).
	Week string `json:"week"`
}

// NewLearningJob returns the tenant's learning job for the week of now. The
// scheduler is the actor, so the job names no requester.
func NewLearningJob(lt LearningTenant, now time.Time) (AgentJob, error) {
	if lt.WorkspaceID == "" {
		return AgentJob{}, invalid("a learning job needs a workspace")
	}
	week := LearningWeek(now)
	input, err := json.Marshal(LearningInput{Trigger: TriggerWeekly, WorkspaceID: lt.WorkspaceID, Week: week})
	if err != nil {
		return AgentJob{}, err
	}
	return NewAgentJob(NewAgentJobInput{
		TenantID:     lt.TenantID,
		AgentType:    AgentLearning,
		DedupKey:     LearningJobDedupKey(lt.TenantID, week),
		InputContext: input,
	}, now)
}

// ParseLearningInput reads a learning job's input context.
func ParseLearningInput(raw json.RawMessage) (LearningInput, error) {
	var in LearningInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return LearningInput{}, invalid("learning input: %v", err)
	}
	if in.Trigger != TriggerWeekly || in.WorkspaceID == "" || in.Week == "" {
		return LearningInput{}, invalid("learning input is incomplete")
	}
	return in, nil
}
