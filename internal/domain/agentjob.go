package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Agent types. A job's AgentType is one of these.
const (
	AgentTriage     = "triage"
	AgentDecomposer = "decomposer"
	AgentBlocker    = "blocker_investigator"
	AgentDigest     = "digest"
	AgentLearning   = "learning"
)

// AgentTypes lists every known agent type.
var AgentTypes = []string{AgentTriage, AgentDecomposer, AgentBlocker, AgentDigest, AgentLearning}

// ValidAgentType reports whether name is a known agent type.
func ValidAgentType(name string) bool { return slices.Contains(AgentTypes, name) }

// SafetyTier decides whether a tool call runs at once or waits for a person.
type SafetyTier int

const (
	// Tier1 runs immediately and is recorded in the task's agent_metadata.
	Tier1 SafetyTier = 1
	// Tier2 is stored as a proposal and runs only after approval.
	Tier2 SafetyTier = 2
)

// JobStatus is an agent job's position in its lifecycle (section 4.3).
type JobStatus string

const (
	JobPending          JobStatus = "pending"
	JobRunning          JobStatus = "running"
	JobAwaitingApproval JobStatus = "awaiting_approval"
	JobCompleted        JobStatus = "completed"
	JobFailed           JobStatus = "failed"
	JobDismissed        JobStatus = "dismissed"
)

// JobStatuses lists every job status.
var JobStatuses = []JobStatus{JobPending, JobRunning, JobAwaitingApproval, JobCompleted, JobFailed, JobDismissed}

// Valid reports whether s is a known job status.
func (s JobStatus) Valid() bool { return slices.Contains(JobStatuses, s) }

// Terminal reports whether no further transition is possible.
func (s JobStatus) Terminal() bool {
	return s == JobCompleted || s == JobFailed || s == JobDismissed
}

// jobTransitions is the agent job state machine in section 4.3.
var jobTransitions = map[JobStatus][]JobStatus{
	JobPending: {JobRunning},
	// running -> pending: a version conflict, or recovery of a job abandoned
	// by a crashed worker (default G11).
	JobRunning:          {JobCompleted, JobAwaitingApproval, JobPending, JobFailed},
	JobAwaitingApproval: {JobCompleted, JobDismissed, JobFailed},
}

// CanTransitionJob reports whether a job may move from one status to another.
func CanTransitionJob(from, to JobStatus) bool {
	return slices.Contains(jobTransitions[from], to)
}

// AgentJob is one unit of agent work. Tenant and task scope come from this
// row, never from model output.
type AgentJob struct {
	ID       uuid.UUID
	TenantID string
	// TaskID is nil for jobs scoped to a user or tenant (digest, learning).
	TaskID    *uuid.UUID
	AgentType string
	Status    JobStatus
	DedupKey  string
	// BaseTaskVersion is the task version the job reasoned about; a proposal
	// is stale once the task has moved past it.
	BaseTaskVersion *int
	InputContext    json.RawMessage
	ProposedActions json.RawMessage
	ExecutionResult json.RawMessage
	ErrorMessage    string
	Attempts        int
	DecidedBy       string
	DecidedAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TransitionTo moves the job to a new status, or returns
// ErrInvalidStateTransition without changing it.
func (j *AgentJob) TransitionTo(to JobStatus, now time.Time) error {
	if !CanTransitionJob(j.Status, to) {
		return fmt.Errorf("%w: agent job %s -> %s", ErrInvalidStateTransition, j.Status, to)
	}
	j.Status = to
	j.UpdatedAt = now.UTC()
	return nil
}

// NewAgentJobInput holds what is needed to queue a job.
type NewAgentJobInput struct {
	TenantID        string
	TaskID          *uuid.UUID
	AgentType       string
	DedupKey        string
	BaseTaskVersion *int
	InputContext    json.RawMessage
}

// NewAgentJob validates the input and returns a pending job.
func NewAgentJob(in NewAgentJobInput, now time.Time) (AgentJob, error) {
	switch {
	case in.TenantID == "":
		return AgentJob{}, invalid("agent job tenant_id is required")
	case !ValidAgentType(in.AgentType):
		return AgentJob{}, invalid("unknown agent type %q", in.AgentType)
	case in.DedupKey == "":
		return AgentJob{}, invalid("agent job dedup_key is required")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return AgentJob{}, err
	}
	ctx := in.InputContext
	if len(ctx) == 0 {
		ctx = json.RawMessage(`{}`)
	}
	now = now.UTC()
	return AgentJob{
		ID:              id,
		TenantID:        in.TenantID,
		TaskID:          in.TaskID,
		AgentType:       in.AgentType,
		Status:          JobPending,
		DedupKey:        in.DedupKey,
		BaseTaskVersion: in.BaseTaskVersion,
		InputContext:    ctx,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// EventJobDedupKey is the dedup key of a job triggered by a task change
// (section 4.4). The same change can only queue one job per agent.
func EventJobDedupKey(agentType string, taskID uuid.UUID, version int) string {
	return fmt.Sprintf("%s:%s:v%d", agentType, taskID, version)
}

// StaleJobDedupKey is the dedup key of a stale-task investigation: at most
// one per task per calendar day in UTC (default G12).
func StaleJobDedupKey(agentType string, taskID uuid.UUID, day time.Time) string {
	return fmt.Sprintf("%s:%s:stale:%s", agentType, taskID, day.UTC().Format("2006-01-02"))
}

// AgentSettings are a workspace's agent controls (section 5.7, decision 10).
type AgentSettings struct {
	AgentsEnabled               bool     `json:"agents_enabled"`
	DisabledAgents              []string `json:"disabled_agents"`
	RequireHITLForAll           bool     `json:"require_hitl_for_all"`
	MaxSubtasksPerDecomposition int      `json:"max_subtasks_per_decomposition"`
}

// DefaultAgentSettings are the settings of a new workspace (decision 19):
// agents off, approval for everything, at most 5 subtasks.
func DefaultAgentSettings() AgentSettings {
	return AgentSettings{
		AgentsEnabled:               false,
		DisabledAgents:              []string{},
		RequireHITLForAll:           true,
		MaxSubtasksPerDecomposition: 5,
	}
}

// ParseAgentSettings reads stored settings. Missing fields take their
// defaults, so a partial document never turns a safety control off.
func ParseAgentSettings(raw []byte) (AgentSettings, error) {
	s := DefaultAgentSettings()
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return AgentSettings{}, invalid("agent_settings: %v", err)
	}
	if s.DisabledAgents == nil {
		s.DisabledAgents = []string{}
	}
	return s, s.Validate()
}

// Validate rejects unknown agent names and a non-positive subtask cap.
func (s AgentSettings) Validate() error {
	for _, a := range s.DisabledAgents {
		if !ValidAgentType(a) {
			return invalid("disabled_agents: unknown agent %q", a)
		}
	}
	if s.MaxSubtasksPerDecomposition <= 0 {
		return invalid("max_subtasks_per_decomposition must be positive")
	}
	return nil
}

// Allows reports whether a job for agentType may be queued or run.
func (s AgentSettings) Allows(agentType string) bool {
	return s.AgentsEnabled && !slices.Contains(s.DisabledAgents, agentType)
}

// AgentSettingsPatch changes some settings; nil fields are left alone.
type AgentSettingsPatch struct {
	AgentsEnabled               *bool     `json:"agents_enabled"`
	DisabledAgents              *[]string `json:"disabled_agents"`
	RequireHITLForAll           *bool     `json:"require_hitl_for_all"`
	MaxSubtasksPerDecomposition *int      `json:"max_subtasks_per_decomposition"`
}

// Apply returns the settings with the patch applied, or a validation error.
// The receiver is not changed.
func (s AgentSettings) Apply(p AgentSettingsPatch) (AgentSettings, error) {
	out := s
	out.DisabledAgents = slices.Clone(s.DisabledAgents)
	if p.AgentsEnabled != nil {
		out.AgentsEnabled = *p.AgentsEnabled
	}
	if p.DisabledAgents != nil {
		out.DisabledAgents = normalizeSet(*p.DisabledAgents)
	}
	if p.RequireHITLForAll != nil {
		out.RequireHITLForAll = *p.RequireHITLForAll
	}
	if p.MaxSubtasksPerDecomposition != nil {
		out.MaxSubtasksPerDecomposition = *p.MaxSubtasksPerDecomposition
	}
	if out.DisabledAgents == nil {
		out.DisabledAgents = []string{}
	}
	if err := out.Validate(); err != nil {
		return s, err
	}
	return out, nil
}
