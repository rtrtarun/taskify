package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

// Decision errors. Each is a 409 for a client: the job exists, but this
// decision cannot be made on it now.
var (
	// ErrStaleProposal means the task changed after the proposal was made.
	// The job is failed with error_message stale_version.
	ErrStaleProposal = errors.New("the task changed after the agent proposed this; the proposal was not applied")
	// ErrAlreadyDecided means the job was already decided the other way, or
	// failed.
	ErrAlreadyDecided = errors.New("this agent job was already decided")
	// ErrNotAwaitingApproval means the job has not reached a decision point.
	ErrNotAwaitingApproval = errors.New("this agent job is not awaiting approval")
)

// StaleVersion is the error_message of a job whose approval came too late.
const StaleVersion = "stale_version"

// ProposalApplier runs a job's approved proposals within the caller's
// transaction. *tools.Registry implements it.
type ProposalApplier interface {
	ApplyProposals(ctx context.Context, job *domain.AgentJob) error
}

// Decider is the person deciding on a proposal.
type Decider struct {
	UserID string
	Admin  bool
}

// AgentJobService lets people read agent jobs and decide on their
// proposals (section 3.7).
type AgentJobService struct {
	tx      repository.TxManager
	jobs    repository.AgentJobRepository
	tasks   repository.TaskRepository
	applier ProposalApplier
	metrics *observability.Metrics
	now     func() time.Time
}

// NewAgentJobService returns an AgentJobService. metrics and now may be nil.
func NewAgentJobService(tx repository.TxManager, jobs repository.AgentJobRepository, tasks repository.TaskRepository,
	applier ProposalApplier, metrics *observability.Metrics, now func() time.Time) *AgentJobService {
	if now == nil {
		now = time.Now
	}
	return &AgentJobService{tx: tx, jobs: jobs, tasks: tasks, applier: applier, metrics: metrics, now: now}
}

func (s *AgentJobService) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Get returns a job in the tenant.
func (s *AgentJobService) Get(ctx context.Context, tenantID string, id uuid.UUID) (*domain.AgentJob, error) {
	return s.jobs.Get(ctx, tenantID, id)
}

type decision int

const (
	approve decision = iota
	reject
)

// Apply approves a job's proposals and applies them, with their outbox
// events, in one transaction that also completes the job. applied is false
// when the job was already approved: a repeated approval changes nothing.
//
// It fails with domain.ErrForbidden for anyone but the task's assignee, its
// creator, or an admin (G10); ErrStaleProposal, after failing the job, when
// the task changed since the proposal; ErrAlreadyDecided when the job was
// dismissed or failed; and ErrNotAwaitingApproval before the job asks. The
// changes are recorded as the agent's, so they never queue agent work.
func (s *AgentJobService) Apply(ctx context.Context, tenantID string, id uuid.UUID, d Decider) (job *domain.AgentJob, applied bool, err error) {
	job, applied, stale, err := s.decide(ctx, tenantID, id, d, approve)
	if err != nil {
		var applyErr *applyError
		if errors.As(err, &applyErr) {
			// The proposals were rolled back together; fail the job so it
			// does not stay waiting on something that cannot apply.
			return s.failApply(ctx, tenantID, id, d, applyErr)
		}
		return nil, false, err
	}
	if stale {
		return job, false, ErrStaleProposal
	}
	return job, applied, nil
}

// Dismiss rejects a job's proposals; no task data changes. dismissed is
// false when the job was already dismissed. The errors are those of Apply,
// with ErrAlreadyDecided for a job that was approved or failed.
func (s *AgentJobService) Dismiss(ctx context.Context, tenantID string, id uuid.UUID, d Decider) (job *domain.AgentJob, dismissed bool, err error) {
	job, dismissed, _, err = s.decide(ctx, tenantID, id, d, reject)
	return job, dismissed, err
}

// applyError wraps a failure of the proposals themselves.
type applyError struct{ err error }

func (e *applyError) Error() string { return e.err.Error() }
func (e *applyError) Unwrap() error { return e.err }

func (s *AgentJobService) decide(ctx context.Context, tenantID string, id uuid.UUID, d Decider, what decision) (
	job *domain.AgentJob, changed, stale bool, err error) {
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		var task *domain.Task
		if job, task, err = s.lockForDecision(ctx, tenantID, id, d); err != nil {
			return err
		}
		switch job.Status {
		case domain.JobAwaitingApproval:
		case domain.JobCompleted:
			if what == approve {
				return nil // already approved: a repeat changes nothing
			}
			return ErrAlreadyDecided
		case domain.JobDismissed:
			if what == reject {
				return nil
			}
			return ErrAlreadyDecided
		case domain.JobFailed:
			if what == approve && job.ErrorMessage == StaleVersion {
				stale = true // the same late approval, again
				return nil
			}
			return ErrAlreadyDecided
		default:
			return ErrNotAwaitingApproval
		}

		now, prev := s.clock(), job.UpdatedAt
		job.DecidedBy, job.DecidedAt = d.UserID, &now
		n := proposalCount(job)
		if what == reject {
			_ = job.TransitionTo(domain.JobDismissed, now)
			if err := s.jobs.Update(ctx, job, domain.JobAwaitingApproval, prev); err != nil {
				return err
			}
			if task != nil {
				if err := s.rejectRecommendation(ctx, job, d.UserID, now); err != nil {
					return err
				}
			}
			changed = true
			s.metrics.AgentProposals(job.AgentType, observability.DecisionRejected, n)
			return nil
		}

		if task != nil && (job.BaseTaskVersion == nil || task.Version != *job.BaseTaskVersion) {
			// Fail safely: nothing is applied, and the failure is kept.
			job.ErrorMessage = StaleVersion
			_ = job.TransitionTo(domain.JobFailed, now)
			if err := s.jobs.Update(ctx, job, domain.JobAwaitingApproval, prev); err != nil {
				return err
			}
			stale = true
			s.metrics.AgentProposals(job.AgentType, observability.DecisionStale, n)
			return nil
		}
		if err := s.applier.ApplyProposals(ctx, job); err != nil {
			return &applyError{err: err}
		}
		_ = job.TransitionTo(domain.JobCompleted, now)
		if err := s.jobs.Update(ctx, job, domain.JobAwaitingApproval, prev); err != nil {
			return err
		}
		changed = true
		s.metrics.AgentProposals(job.AgentType, observability.DecisionApproved, n)
		return nil
	})
	if err != nil {
		return nil, false, false, err
	}
	return job, changed, stale, nil
}

// rejectRecommendation stops a dismissed job's priority recommendation from
// steering the task's notifications, keeping it for audit (G25). The
// task's version does not move: nothing a person edits has changed.
func (s *AgentJobService) rejectRecommendation(ctx context.Context, job *domain.AgentJob, by string, now time.Time) error {
	task, err := s.tasks.GetForUpdate(ctx, job.TenantID, *job.TaskID)
	if err != nil {
		return err
	}
	if !task.RejectPriorityRecommendation(job.ID, by, now) {
		return nil
	}
	return s.tasks.Update(ctx, task, task.Version)
}

// lockForDecision locks the job and checks that d may decide on it.
func (s *AgentJobService) lockForDecision(ctx context.Context, tenantID string, id uuid.UUID, d Decider) (*domain.AgentJob, *domain.Task, error) {
	job, err := s.jobs.GetForUpdate(ctx, tenantID, id)
	if err != nil {
		return nil, nil, err
	}
	if job.TaskID == nil {
		// No task to own the decision: admins only.
		if !d.Admin {
			return nil, nil, fmt.Errorf("%w: only an admin may decide on this job", domain.ErrForbidden)
		}
		return job, nil, nil
	}
	task, err := s.tasks.Get(ctx, tenantID, *job.TaskID)
	if err != nil {
		return nil, nil, err
	}
	if !task.CanDecide(d.UserID, d.Admin) {
		return nil, nil, fmt.Errorf("%w: only the task's assignee, its creator, or an admin may decide", domain.ErrForbidden)
	}
	return job, task, nil
}

// failApply records that approved proposals could not be applied. A version
// conflict while applying means the task moved under the approval: stale.
func (s *AgentJobService) failApply(ctx context.Context, tenantID string, id uuid.UUID, d Decider, cause *applyError) (*domain.AgentJob, bool, error) {
	stale := errors.Is(cause, domain.ErrVersionConflict)
	var job *domain.AgentJob
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if job, _, err = s.lockForDecision(ctx, tenantID, id, d); err != nil {
			return err
		}
		if job.Status != domain.JobAwaitingApproval {
			return nil // decided meanwhile
		}
		now, prev := s.clock(), job.UpdatedAt
		job.DecidedBy, job.DecidedAt = d.UserID, &now
		job.ErrorMessage = "apply failed: " + cause.Error()
		if stale {
			job.ErrorMessage = StaleVersion
		}
		_ = job.TransitionTo(domain.JobFailed, now)
		return s.jobs.Update(ctx, job, domain.JobAwaitingApproval, prev)
	})
	if err != nil {
		return nil, false, err
	}
	if stale {
		s.metrics.AgentProposals(job.AgentType, observability.DecisionStale, proposalCount(job))
		return job, false, ErrStaleProposal
	}
	return job, false, fmt.Errorf("%w: the approved changes could not be applied: %s", domain.ErrValidation, cause.Error())
}

func proposalCount(job *domain.AgentJob) int {
	var list []json.RawMessage
	_ = json.Unmarshal(job.ProposedActions, &list)
	return len(list)
}
