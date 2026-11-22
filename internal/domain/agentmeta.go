package domain

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
)

// MaxAgentRuns is how many agent changes a task's agent_metadata keeps; the
// oldest are dropped first.
const MaxAgentRuns = 20

// AgentMetadata is what agents recorded on a task (section 5.7).
type AgentMetadata struct {
	// Runs lists the Tier 1 changes agents made, oldest first.
	Runs []AgentRunRecord `json:"runs,omitempty"`
	// PriorityRecommendation is an agent's suggested priority, proposed for
	// approval. Notifications use it as their priority hint until a person
	// rejects it.
	PriorityRecommendation *PriorityRecommendation `json:"priority_recommendation,omitempty"`
	// RejectedRecommendations keeps rejected recommendations for audit,
	// oldest first, the last MaxAgentRuns of them.
	RejectedRecommendations []RejectedRecommendation `json:"rejected_priority_recommendations,omitempty"`
}

// RejectedRecommendation is a priority recommendation a person rejected.
type RejectedRecommendation struct {
	PriorityRecommendation
	RejectedBy string    `json:"rejected_by"`
	RejectedAt time.Time `json:"rejected_at"`
}

// AgentRunRecord is one agent change and its reason.
type AgentRunRecord struct {
	JobID     uuid.UUID      `json:"job_id"`
	Agent     string         `json:"agent"`
	At        time.Time      `json:"at"`
	Changes   map[string]any `json:"changes"`
	Reasoning string         `json:"reasoning"`
}

// PriorityRecommendation is a priority an agent suggests.
type PriorityRecommendation struct {
	Value     Priority  `json:"value"`
	JobID     uuid.UUID `json:"job_id"`
	Reasoning string    `json:"reasoning,omitempty"`
	At        time.Time `json:"at"`
}

// ParseAgentMetadata reads stored agent_metadata.
func ParseAgentMetadata(raw []byte) (AgentMetadata, error) {
	var m AgentMetadata
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return AgentMetadata{}, invalid("agent_metadata: %v", err)
	}
	return m, nil
}

// Clone returns a copy that shares nothing with m.
func (m AgentMetadata) Clone() AgentMetadata {
	c := AgentMetadata{Runs: make([]AgentRunRecord, len(m.Runs))}
	for i, r := range m.Runs {
		r.Changes = maps.Clone(r.Changes)
		c.Runs[i] = r
	}
	if len(c.Runs) == 0 {
		c.Runs = nil
	}
	if m.PriorityRecommendation != nil {
		rec := *m.PriorityRecommendation
		c.PriorityRecommendation = &rec
	}
	c.RejectedRecommendations = slices.Clone(m.RejectedRecommendations)
	return c
}

// RecordAgentRun appends an agent change, keeping the last MaxAgentRuns.
func (t *Task) RecordAgentRun(r AgentRunRecord) {
	r.At = r.At.UTC()
	r.Changes = maps.Clone(r.Changes)
	t.AgentMetadata.Runs = append(t.AgentMetadata.Runs, r)
	if n := len(t.AgentMetadata.Runs); n > MaxAgentRuns {
		t.AgentMetadata.Runs = slices.Clone(t.AgentMetadata.Runs[n-MaxAgentRuns:])
	}
}

// RunCommentID is the key under which a run's Changes name the comment the
// agent added.
const RunCommentID = "comment_id"

// CommentOf returns the comment a job added to the task, if its run recorded
// one.
func (t *Task) CommentOf(jobID uuid.UUID) (uuid.UUID, bool) {
	for _, r := range t.AgentMetadata.Runs {
		if r.JobID != jobID {
			continue
		}
		if s, ok := r.Changes[RunCommentID].(string); ok {
			if id, err := uuid.Parse(s); err == nil {
				return id, true
			}
		}
	}
	return uuid.Nil, false
}

// RecommendPriority stores an agent's suggested priority. It changes no
// field a person edits, so it does not move the task's version.
func (t *Task) RecommendPriority(rec PriorityRecommendation) error {
	if !rec.Value.Valid() {
		return invalid("unknown priority %q", rec.Value)
	}
	rec.At = rec.At.UTC()
	t.AgentMetadata.PriorityRecommendation = &rec
	return nil
}

// RejectPriorityRecommendation clears the active recommendation if it came
// from jobID, so later notifications fall back to the task's own priority,
// and keeps it among the rejected ones for audit (G25). A recommendation
// from another job is left alone. It reports whether anything changed. Like
// RecommendPriority, it does not move the task's version.
func (t *Task) RejectPriorityRecommendation(jobID uuid.UUID, by string, at time.Time) bool {
	rec := t.AgentMetadata.PriorityRecommendation
	if rec == nil || rec.JobID != jobID {
		return false
	}
	t.AgentMetadata.RejectedRecommendations = append(t.AgentMetadata.RejectedRecommendations,
		RejectedRecommendation{PriorityRecommendation: *rec, RejectedBy: by, RejectedAt: at.UTC()})
	if n := len(t.AgentMetadata.RejectedRecommendations); n > MaxAgentRuns {
		t.AgentMetadata.RejectedRecommendations = slices.Clone(t.AgentMetadata.RejectedRecommendations[n-MaxAgentRuns:])
	}
	t.AgentMetadata.PriorityRecommendation = nil
	return true
}
