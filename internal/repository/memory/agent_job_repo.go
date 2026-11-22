package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// AgentJobRepo implements repository.AgentJobRepository.
type AgentJobRepo struct{ st *Store }

// AgentJobs returns the agent job repository.
func (st *Store) AgentJobs() *AgentJobRepo { return &AgentJobRepo{st} }

var _ repository.AgentJobRepository = (*AgentJobRepo)(nil)

// copyJob returns a copy sharing no byte slices.
func copyJob(j *domain.AgentJob) *domain.AgentJob {
	c := *j
	c.InputContext = slices.Clone(j.InputContext)
	c.ProposedActions = slices.Clone(j.ProposedActions)
	c.ExecutionResult = slices.Clone(j.ExecutionResult)
	if j.TaskID != nil {
		id := *j.TaskID
		c.TaskID = &id
	}
	if j.BaseTaskVersion != nil {
		v := *j.BaseTaskVersion
		c.BaseTaskVersion = &v
	}
	c.DecidedAt = microPtr(j.DecidedAt)
	c.RetryAfter = microPtr(j.RetryAfter)
	return &c
}

func (r *AgentJobRepo) Insert(ctx context.Context, j domain.AgentJob) (created bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		// The same checks the schema makes.
		if j.TaskID != nil && s.tasks[*j.TaskID] == nil {
			return fmt.Errorf("%w: agent job for a missing task", domain.ErrValidation)
		}
		if j.Attempts < 0 || !j.Status.Valid() {
			return fmt.Errorf("%w: invalid agent job", domain.ErrValidation)
		}
		if _, ok := s.jobKeys[j.DedupKey]; ok {
			return nil
		}
		if _, ok := s.jobs[j.ID]; ok {
			return fmt.Errorf("%w: agent job %s", domain.ErrAlreadyExists, j.ID)
		}
		c := copyJob(&j)
		c.RetryAfter, c.ConflictCount = nil, 0 // as the insert leaves them to their defaults
		if len(c.InputContext) == 0 {
			c.InputContext = []byte(`{}`)
		}
		c.CreatedAt, c.UpdatedAt = micro(c.CreatedAt), micro(c.UpdatedAt)
		s.jobs[c.ID] = c
		s.jobKeys[c.DedupKey] = c.ID
		created = true
		return nil
	})
	return created, err
}

func (r *AgentJobRepo) Get(ctx context.Context, tenantID string, id uuid.UUID) (out *domain.AgentJob, err error) {
	err = r.st.do(ctx, func(s *state) error {
		j, ok := s.jobs[id]
		if !ok || j.TenantID != tenantID {
			return domain.ErrNotFound
		}
		out = copyJob(j)
		return nil
	})
	return out, err
}

func (r *AgentJobRepo) GetForUpdate(ctx context.Context, tenantID string, id uuid.UUID) (*domain.AgentJob, error) {
	if !inTx(ctx) {
		return nil, repository.ErrTxRequired
	}
	return r.Get(ctx, tenantID, id)
}

func (r *AgentJobRepo) Claim(ctx context.Context, now time.Time, limit int, agentTypes []string) (out []*domain.AgentJob, err error) {
	out = []*domain.AgentJob{}
	err = r.st.do(ctx, func(s *state) error {
		var pending []*domain.AgentJob
		for _, j := range s.jobs {
			due := j.RetryAfter == nil || !j.RetryAfter.After(now)
			if j.Status == domain.JobPending && due && slices.Contains(agentTypes, j.AgentType) {
				pending = append(pending, j)
			}
		}
		sortJobs(pending)
		for _, j := range pending[:min(limit, len(pending))] {
			j.Status, j.UpdatedAt = domain.JobRunning, micro(now)
			out = append(out, copyJob(j))
		}
		return nil
	})
	return out, err
}

func (r *AgentJobRepo) Update(ctx context.Context, j *domain.AgentJob, from domain.JobStatus, fromUpdatedAt time.Time) error {
	return r.st.do(ctx, func(s *state) error {
		stored, ok := s.jobs[j.ID]
		if !ok || stored.TenantID != j.TenantID {
			return domain.ErrNotFound
		}
		if stored.Status != from || !stored.UpdatedAt.Equal(micro(fromUpdatedAt)) {
			return repository.ErrJobMoved
		}
		if j.Attempts < 0 || j.ConflictCount < 0 || !j.Status.Valid() || (j.DecidedBy == "") != (j.DecidedAt == nil) {
			return fmt.Errorf("%w: invalid agent job", domain.ErrValidation)
		}
		c := copyJob(j)
		stored.Status, stored.ProposedActions, stored.ExecutionResult = c.Status, c.ProposedActions, c.ExecutionResult
		stored.ErrorMessage, stored.Attempts = c.ErrorMessage, c.Attempts
		stored.DecidedBy, stored.DecidedAt, stored.UpdatedAt = c.DecidedBy, c.DecidedAt, micro(c.UpdatedAt)
		stored.BaseTaskVersion = c.BaseTaskVersion
		stored.RetryAfter, stored.ConflictCount = c.RetryAfter, c.ConflictCount
		return nil
	})
}

func (r *AgentJobRepo) RecoverStuck(ctx context.Context, olderThan, now time.Time, maxAttempts int) (requeued, failed int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for _, j := range s.jobs {
			if j.Status != domain.JobRunning || !j.UpdatedAt.Before(olderThan) {
				continue
			}
			j.Attempts++
			j.UpdatedAt = micro(now)
			if j.Attempts >= maxAttempts {
				j.Status, j.ErrorMessage = domain.JobFailed, "abandoned while running; attempt limit reached"
				failed++
			} else {
				j.Status = domain.JobPending
				requeued++
			}
		}
		return nil
	})
	return requeued, failed, err
}

func (r *AgentJobRepo) ListForTask(ctx context.Context, tenantID string, taskID uuid.UUID) ([]*domain.AgentJob, error) {
	out := []*domain.AgentJob{}
	err := r.st.do(ctx, func(s *state) error {
		for _, j := range s.jobs {
			if j.TenantID == tenantID && j.TaskID != nil && *j.TaskID == taskID {
				out = append(out, copyJob(j))
			}
		}
		return nil
	})
	sortJobs(out)
	return out, err
}

func (r *AgentJobRepo) ListAwaitingApproval(ctx context.Context, tenantID, userID string) ([]*domain.AgentJob, error) {
	out := []*domain.AgentJob{}
	err := r.st.do(ctx, func(s *state) error {
		for _, j := range s.jobs {
			if j.TenantID != tenantID || j.Status != domain.JobAwaitingApproval || j.TaskID == nil {
				continue
			}
			if t := s.tasks[*j.TaskID]; t != nil && t.TenantID == tenantID && t.Owner() == userID {
				out = append(out, copyJob(j))
			}
		}
		return nil
	})
	sortJobs(out)
	return out, err
}

func (r *AgentJobRepo) LatestBrief(ctx context.Context, tenantID, userID string) (*domain.AgentJob, error) {
	var briefs []*domain.AgentJob
	err := r.st.do(ctx, func(s *state) error {
		for _, j := range s.jobs {
			if j.TenantID != tenantID || j.AgentType != domain.AgentDigest || j.Status != domain.JobCompleted {
				continue
			}
			var in struct {
				UserID string `json:"user_id"`
			}
			var res map[string]json.RawMessage
			if json.Unmarshal(j.InputContext, &in) != nil || in.UserID != userID ||
				json.Unmarshal(j.ExecutionResult, &res) != nil || res["report"] == nil {
				continue
			}
			briefs = append(briefs, copyJob(j))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(briefs) == 0 {
		return nil, domain.ErrNotFound
	}
	sortJobs(briefs)
	return briefs[len(briefs)-1], nil
}

// LockBriefs needs no lock of its own: a transaction already holds the
// store's.
func (r *AgentJobRepo) LockBriefs(ctx context.Context, _ string, _ bool) error {
	if !inTx(ctx) {
		return repository.ErrTxRequired
	}
	return nil
}

func (r *AgentJobRepo) DeleteBriefsMentioning(ctx context.Context, tenantID string, taskID uuid.UUID) (ids []uuid.UUID, err error) {
	ids = []uuid.UUID{}
	err = r.st.do(ctx, func(s *state) error {
		for id, j := range s.jobs {
			if j.TenantID != tenantID || j.AgentType != domain.AgentDigest || j.TaskID != nil {
				continue
			}
			var res map[string]json.RawMessage
			if json.Unmarshal(j.ExecutionResult, &res) != nil || !domain.BriefMentions(res["report"], taskID) {
				continue
			}
			delete(s.jobs, id)
			delete(s.jobKeys, j.DedupKey)
			s.runs = slices.DeleteFunc(s.runs, func(r domain.AgentRun) bool { return r.JobID == id })
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

func (r *AgentJobRepo) InsertRun(ctx context.Context, run domain.AgentRun) error {
	return r.st.do(ctx, func(s *state) error {
		if s.jobs[run.JobID] == nil {
			return fmt.Errorf("%w: run for a missing agent job", domain.ErrValidation)
		}
		if run.Turn < 1 || run.Outcome == "" {
			return fmt.Errorf("%w: invalid agent run", domain.ErrValidation)
		}
		run.ToolArgs, run.ToolResult = slices.Clone(run.ToolArgs), slices.Clone(run.ToolResult)
		run.CreatedAt = micro(run.CreatedAt)
		s.runs = append(s.runs, run)
		return nil
	})
}

func (r *AgentJobRepo) ListRuns(ctx context.Context, jobID uuid.UUID) ([]domain.AgentRun, error) {
	out := []domain.AgentRun{}
	err := r.st.do(ctx, func(s *state) error {
		for _, run := range s.runs {
			if run.JobID == jobID {
				run.ToolArgs, run.ToolResult = slices.Clone(run.ToolArgs), slices.Clone(run.ToolResult)
				out = append(out, run)
			}
		}
		return nil
	})
	slices.SortStableFunc(out, func(a, b domain.AgentRun) int { return a.Turn - b.Turn })
	return out, err
}

// deleteJobsForTask removes a task's jobs and their runs, as ON DELETE
// CASCADE does.
func (s *state) deleteJobsForTask(taskID uuid.UUID) {
	for id, j := range s.jobs {
		if j.TaskID != nil && *j.TaskID == taskID {
			delete(s.jobKeys, j.DedupKey)
			delete(s.jobs, id)
			s.runs = slices.DeleteFunc(s.runs, func(r domain.AgentRun) bool { return r.JobID == id })
		}
	}
}

// sortJobs orders jobs as PostgreSQL does: by created_at, then id.
func sortJobs(jobs []*domain.AgentJob) {
	slices.SortFunc(jobs, func(a, b *domain.AgentJob) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	})
}
