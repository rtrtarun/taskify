package domain

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestEstimate(t *testing.T) {
	task := newTaskT(t, "T", "usr_bob", PriorityLow)
	ninety := 90
	events, err := task.Apply(Patch{EstimatedMinutes: &ninety}, alice, t0)
	if err != nil || task.Version != 2 || *task.EstimatedMinutes != 90 || len(events) != 0 {
		t.Fatalf("set estimate: v%d %v %v", task.Version, events, err)
	}
	// The same estimate again changes nothing.
	if events, _ := task.Apply(Patch{EstimatedMinutes: &ninety}, alice, t0); events != nil || task.Version != 2 {
		t.Errorf("repeat changed the task: v%d", task.Version)
	}
	for _, bad := range []int{0, -5, maxEstimateMinutes + 1} {
		v := bad
		if _, err := task.Apply(Patch{EstimatedMinutes: &v}, alice, t0); !errors.Is(err, ErrValidation) {
			t.Errorf("estimate %d: %v", bad, err)
		}
	}
	// A clone does not share the estimate.
	c := task.clone()
	*c.EstimatedMinutes = 1
	if *task.EstimatedMinutes != 90 {
		t.Error("clone shares the estimate")
	}
}

func TestAgentMetadata(t *testing.T) {
	task := newTaskT(t, "T", "usr_bob", PriorityLow)
	for i := range MaxAgentRuns + 3 {
		task.RecordAgentRun(AgentRunRecord{JobID: uuid.New(), Agent: AgentTriage, At: t0,
			Changes: map[string]any{"n": i}, Reasoning: "because"})
	}
	runs := task.AgentMetadata.Runs
	if len(runs) != MaxAgentRuns || runs[0].Changes["n"] != 3 || runs[len(runs)-1].Changes["n"] != MaxAgentRuns+2 {
		t.Errorf("runs kept: %d, first %v", len(runs), runs[0].Changes)
	}
	if err := task.RecommendPriority(PriorityRecommendation{Value: "urgent"}); !errors.Is(err, ErrValidation) {
		t.Errorf("invalid recommendation: %v", err)
	}
	if err := task.RecommendPriority(PriorityRecommendation{Value: PriorityHigh, JobID: uuid.New(), Reasoning: "outage", At: t0}); err != nil {
		t.Fatal(err)
	}
	// Round trip through JSON, as stored.
	raw, _ := json.Marshal(task.AgentMetadata)
	back, err := ParseAgentMetadata(raw)
	if err != nil || len(back.Runs) != MaxAgentRuns || back.PriorityRecommendation.Value != PriorityHigh {
		t.Errorf("round trip = %+v, %v", back, err)
	}
	if m, err := ParseAgentMetadata([]byte(`{}`)); err != nil || m.Runs != nil || m.PriorityRecommendation != nil {
		t.Errorf("empty = %+v, %v", m, err)
	}
	if _, err := ParseAgentMetadata([]byte(`{"runs": 5}`)); !errors.Is(err, ErrValidation) {
		t.Errorf("bad metadata: %v", err)
	}
	// A clone does not share metadata.
	c := task.clone()
	c.AgentMetadata.Runs[0].Changes["n"] = "changed"
	c.AgentMetadata.PriorityRecommendation.Value = PriorityLow
	if task.AgentMetadata.Runs[0].Changes["n"] == "changed" || task.AgentMetadata.PriorityRecommendation.Value != PriorityHigh {
		t.Error("clone shares agent metadata")
	}
}

func TestRecommendationIsThePriorityHint(t *testing.T) {
	task := newTaskT(t, "T", "usr_bob", PriorityLow)
	hint := func(typ string) string { return task.event(typ, alice, t0).priorityHint() }
	if hint(EventTaskUpdated) != "low" {
		t.Fatalf("before = %s", hint(EventTaskUpdated))
	}
	_ = task.RecommendPriority(PriorityRecommendation{Value: PriorityCritical, At: t0})
	if got := hint(EventTaskUpdated); got != "critical" {
		t.Errorf("with a critical recommendation = %s", got)
	}
	// A manual block is still at least high when the recommendation is lower.
	_ = task.RecommendPriority(PriorityRecommendation{Value: PriorityMedium, At: t0})
	reason := BlockedManual
	task.BlockedReason = &reason
	if got := hint(EventTaskBlocked); got != "high" {
		t.Errorf("manual block with a medium recommendation = %s", got)
	}
}
