package domain

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestJobTransitionMatrix(t *testing.T) {
	allowed := map[JobStatus][]JobStatus{
		JobPending:          {JobRunning},
		JobRunning:          {JobCompleted, JobAwaitingApproval, JobPending, JobFailed},
		JobAwaitingApproval: {JobCompleted, JobDismissed, JobFailed},
		JobCompleted:        nil,
		JobFailed:           nil,
		JobDismissed:        nil,
	}
	for _, from := range JobStatuses {
		for _, to := range JobStatuses {
			want := slices.Contains(allowed[from], to)
			if got := CanTransitionJob(from, to); got != want {
				t.Errorf("%s -> %s: got %v, want %v", from, to, got, want)
			}
			j := AgentJob{Status: from}
			err := j.TransitionTo(to, t0)
			switch {
			case want && err != nil:
				t.Errorf("%s -> %s: unexpected error %v", from, to, err)
			case want && j.Status != to:
				t.Errorf("%s -> %s: status is %s", from, to, j.Status)
			case !want && !errors.Is(err, ErrInvalidStateTransition):
				t.Errorf("%s -> %s: err = %v, want ErrInvalidStateTransition", from, to, err)
			case !want && j.Status != from:
				t.Errorf("%s -> %s: a refused transition changed the status", from, to)
			}
		}
		if from.Terminal() != (len(allowed[from]) == 0) {
			t.Errorf("%s.Terminal() = %v", from, from.Terminal())
		}
	}
}

func TestNewAgentJob(t *testing.T) {
	task := uuid.New()
	v := 3
	j, err := NewAgentJob(NewAgentJobInput{
		TenantID: "tenant_acme", TaskID: &task, AgentType: AgentTriage,
		DedupKey: EventJobDedupKey(AgentTriage, task, v), BaseTaskVersion: &v,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != JobPending || j.Attempts != 0 || string(j.InputContext) != "{}" || j.ID == uuid.Nil {
		t.Errorf("job = %+v", j)
	}
	for name, in := range map[string]NewAgentJobInput{
		"no tenant":     {AgentType: AgentTriage, DedupKey: "k"},
		"unknown agent": {TenantID: "t", AgentType: "oracle", DedupKey: "k"},
		"no dedup key":  {TenantID: "t", AgentType: AgentTriage},
	} {
		if _, err := NewAgentJob(in, t0); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

func TestJobDedupKeys(t *testing.T) {
	id := uuid.MustParse("01930000-0000-7000-8000-000000000001")
	if got, want := EventJobDedupKey(AgentTriage, id, 4), "triage:"+id.String()+":v4"; got != want {
		t.Errorf("event key = %q, want %q", got, want)
	}
	late := time.Date(2026, 11, 11, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600))
	if got, want := StaleJobDedupKey(AgentBlocker, id, late), "blocker_investigator:"+id.String()+":stale:2026-11-12"; got != want {
		t.Errorf("stale key = %q, want %q (day is taken in UTC)", got, want)
	}
}

func TestDefaultAgentSettings(t *testing.T) {
	s := DefaultAgentSettings()
	if s.AgentsEnabled || !s.RequireHITLForAll || s.MaxSubtasksPerDecomposition != 5 || s.DisabledAgents == nil || len(s.DisabledAgents) != 0 {
		t.Errorf("defaults = %+v", s)
	}
	for _, a := range AgentTypes {
		if s.Allows(a) {
			t.Errorf("a new workspace allows %s", a)
		}
	}
	// The database default must parse to the same settings.
	db := `{"agents_enabled": false, "disabled_agents": [], "require_hitl_for_all": true, "max_subtasks_per_decomposition": 5}`
	parsed, err := ParseAgentSettings([]byte(db))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(parsed.DisabledAgents, s.DisabledAgents) || parsed.AgentsEnabled != s.AgentsEnabled ||
		parsed.RequireHITLForAll != s.RequireHITLForAll || parsed.MaxSubtasksPerDecomposition != s.MaxSubtasksPerDecomposition {
		t.Errorf("database default parses to %+v, want %+v", parsed, s)
	}
}

func TestParseAgentSettings(t *testing.T) {
	// A partial document keeps the safe defaults for missing fields.
	s, err := ParseAgentSettings([]byte(`{"agents_enabled": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.AgentsEnabled || !s.RequireHITLForAll || s.MaxSubtasksPerDecomposition != 5 {
		t.Errorf("partial = %+v", s)
	}
	if !s.Allows(AgentTriage) {
		t.Error("enabled settings refuse triage")
	}
	for name, raw := range map[string]string{
		"not json":      `{`,
		"unknown agent": `{"disabled_agents": ["oracle"]}`,
		"zero cap":      `{"max_subtasks_per_decomposition": 0}`,
	} {
		if _, err := ParseAgentSettings([]byte(raw)); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

func TestAgentSettingsPatch(t *testing.T) {
	on, off := true, false
	disabled := []string{" decomposer ", AgentDigest, AgentDecomposer}
	s, err := DefaultAgentSettings().Apply(AgentSettingsPatch{AgentsEnabled: &on, RequireHITLForAll: &off, DisabledAgents: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if !s.AgentsEnabled || s.RequireHITLForAll || !slices.Equal(s.DisabledAgents, []string{AgentDecomposer, AgentDigest}) {
		t.Errorf("patched = %+v", s)
	}
	if !s.Allows(AgentTriage) || s.Allows(AgentDecomposer) {
		t.Error("disabled_agents is not honoured")
	}

	bad := []string{"oracle"}
	zero, neg := 0, -1
	for name, p := range map[string]AgentSettingsPatch{
		"unknown agent": {DisabledAgents: &bad},
		"zero cap":      {MaxSubtasksPerDecomposition: &zero},
		"negative cap":  {MaxSubtasksPerDecomposition: &neg},
	} {
		got, err := s.Apply(p)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
		if !slices.Equal(got.DisabledAgents, s.DisabledAgents) || got.MaxSubtasksPerDecomposition != s.MaxSubtasksPerDecomposition {
			t.Errorf("%s: a refused patch changed the settings", name)
		}
	}
}

func TestAgentTriggers(t *testing.T) {
	agent := Actor{ID: "agent_triage", Kind: ActorAgent}
	task := newTaskT(t, "T", "usr_bob", "")
	blockedBy := func(r BlockedReason) Event {
		e := task.event(EventTaskBlocked, alice, t0)
		e.Task.BlockedReason = &r
		return e
	}
	manual, dependency := blockedBy(BlockedManual), blockedBy(BlockedDependency)

	names := func(ts []AgentTrigger) []string {
		out := []string{}
		for _, tr := range ts {
			out = append(out, tr.AgentType+"@"+tr.Event)
		}
		return out
	}
	if got := names(CreateTriggers(alice)); !slices.Equal(got, []string{"triage@task.created"}) {
		t.Errorf("created by a person: %v", got)
	}
	for _, a := range []Actor{agent, System} {
		if got := CreateTriggers(a); got != nil {
			t.Errorf("created by %s: %v", a.Kind, got)
		}
	}
	for _, tc := range []struct {
		name   string
		events []Event
		want   []string
	}{
		{"no events", nil, []string{}},
		{"edited by a person", []Event{task.event(EventTaskUpdated, alice, t0)}, []string{"triage@task.updated"}},
		{"edited by an agent", []Event{task.event(EventTaskUpdated, agent, t0)}, []string{}},
		{"edited by the system", []Event{task.event(EventTaskUpdated, System, t0)}, []string{}},
		{"manual block", []Event{manual}, []string{"blocker_investigator@task.blocked"}},
		{"dependency block", []Event{dependency}, []string{}},
		{"several events, one job per agent",
			[]Event{task.event(EventTaskAssigned, alice, t0), manual, task.event(EventTaskUpdated, alice, t0), task.event(EventTaskUpdated, alice, t0)},
			[]string{"blocker_investigator@task.blocked", "triage@task.updated"}},
		{"assignment and completion queue nothing",
			[]Event{task.event(EventTaskAssigned, alice, t0), task.event(EventTaskCompleted, alice, t0)}, []string{}},
	} {
		if got := names(ChangeTriggers(tc.events)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: triggers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
