package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// AgentJobRepo implements repository.AgentJobRepository.
type AgentJobRepo struct{ db *DB }

// NewAgentJobRepo returns an agent job repository on db.
func NewAgentJobRepo(db *DB) *AgentJobRepo { return &AgentJobRepo{db: db} }

var _ repository.AgentJobRepository = (*AgentJobRepo)(nil)

const jobColumns = `id, tenant_id, task_id, agent_type, status::text, dedup_key, base_task_version,
	input_context, proposed_actions, execution_result, error_message, attempts, decided_by, decided_at,
	created_at, updated_at, retry_after, conflict_count`

func (r *AgentJobRepo) Insert(ctx context.Context, j domain.AgentJob) (bool, error) {
	// ON CONFLICT rather than catching the unique violation: an error would
	// abort the task transaction the job is written in.
	tag, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO agent_jobs (id, tenant_id, task_id, agent_type, status, dedup_key, base_task_version,
			input_context, attempts, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5::text::agent_job_status, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (dedup_key) DO NOTHING`,
		j.ID, j.TenantID, j.TaskID, j.AgentType, string(j.Status), j.DedupKey, j.BaseTaskVersion,
		jsonObject(j.InputContext), j.Attempts, j.CreatedAt, j.UpdatedAt)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *AgentJobRepo) Get(ctx context.Context, tenantID string, id uuid.UUID) (*domain.AgentJob, error) {
	return r.one(ctx, `SELECT `+jobColumns+` FROM agent_jobs WHERE id = $1 AND tenant_id = $2`, id, tenantID)
}

func (r *AgentJobRepo) GetForUpdate(ctx context.Context, tenantID string, id uuid.UUID) (*domain.AgentJob, error) {
	if _, err := r.db.tx(ctx); err != nil {
		return nil, err
	}
	return r.one(ctx, `SELECT `+jobColumns+` FROM agent_jobs WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID)
}

func (r *AgentJobRepo) Claim(ctx context.Context, now time.Time, limit int, agentTypes []string) ([]*domain.AgentJob, error) {
	if len(agentTypes) == 0 {
		return []*domain.AgentJob{}, nil
	}
	// One statement, as for the outbox: the subquery locks pending rows,
	// skipping those another claimer holds, and the update marks them.
	return r.query(ctx, `
		UPDATE agent_jobs SET status = 'running', updated_at = $1
		WHERE id IN (
			SELECT id FROM agent_jobs
			WHERE status = 'pending' AND agent_type = ANY($3) AND (retry_after IS NULL OR retry_after <= $1)
			ORDER BY created_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING `+jobColumns, now.UTC().Truncate(time.Microsecond), limit, agentTypes)
}

func (r *AgentJobRepo) Update(ctx context.Context, j *domain.AgentJob, from domain.JobStatus, fromUpdatedAt time.Time) error {
	tag, err := r.db.q(ctx).Exec(ctx, `
		UPDATE agent_jobs
		SET status = $4::text::agent_job_status, proposed_actions = $5, execution_result = $6,
			error_message = $7, attempts = $8, decided_by = $9, decided_at = $10, updated_at = $11,
			base_task_version = $13, retry_after = $14, conflict_count = $15
		WHERE id = $1 AND tenant_id = $2 AND status = $3::text::agent_job_status AND updated_at = $12`,
		j.ID, j.TenantID, string(from), string(j.Status), jsonOrNull(j.ProposedActions), jsonOrNull(j.ExecutionResult),
		nullString(truncateErr(j.ErrorMessage)), j.Attempts, nullString(j.DecidedBy), j.DecidedAt,
		j.UpdatedAt.UTC().Truncate(time.Microsecond), fromUpdatedAt.UTC().Truncate(time.Microsecond), j.BaseTaskVersion,
		j.RetryAfter, j.ConflictCount)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.Get(ctx, j.TenantID, j.ID); err != nil {
		return err
	}
	return repository.ErrJobMoved
}

func (r *AgentJobRepo) RecoverStuck(ctx context.Context, olderThan, now time.Time, maxAttempts int) (requeued, failed int, err error) {
	err = r.db.WithTx(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)
		now := now.UTC().Truncate(time.Microsecond)
		tag, err := q.Exec(ctx, `
			UPDATE agent_jobs
			SET status = 'failed', attempts = attempts + 1, updated_at = $2,
				error_message = 'abandoned while running; attempt limit reached'
			WHERE status = 'running' AND updated_at < $1 AND attempts + 1 >= $3`, olderThan, now, maxAttempts)
		if err != nil {
			return mapErr(err)
		}
		failed = int(tag.RowsAffected())
		tag, err = q.Exec(ctx, `
			UPDATE agent_jobs SET status = 'pending', attempts = attempts + 1, updated_at = $2
			WHERE status = 'running' AND updated_at < $1`, olderThan, now)
		if err != nil {
			return mapErr(err)
		}
		requeued = int(tag.RowsAffected())
		return nil
	})
	return requeued, failed, err
}

func (r *AgentJobRepo) ListForTask(ctx context.Context, tenantID string, taskID uuid.UUID) ([]*domain.AgentJob, error) {
	return r.query(ctx, `SELECT `+jobColumns+` FROM agent_jobs
		WHERE tenant_id = $1 AND task_id = $2 ORDER BY created_at, id`, tenantID, taskID)
}

func (r *AgentJobRepo) InsertRun(ctx context.Context, run domain.AgentRun) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO agent_run_history (id, job_id, turn, model, tool_name, tool_args, tool_result, outcome,
			prompt_tokens, completion_tokens, latency_ms, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		run.ID, run.JobID, run.Turn, run.Model, nullString(run.ToolName), jsonOrNull(run.ToolArgs),
		jsonOrNull(run.ToolResult), run.Outcome, run.PromptTokens, run.CompletionTokens, run.LatencyMS, run.CreatedAt)
	return mapErr(err)
}

func (r *AgentJobRepo) ListRuns(ctx context.Context, jobID uuid.UUID) ([]domain.AgentRun, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT id, job_id, turn, model, tool_name, tool_args, tool_result, outcome,
			prompt_tokens, completion_tokens, latency_ms, created_at
		FROM agent_run_history WHERE job_id = $1 ORDER BY turn, created_at, id`, jobID)
	if err != nil {
		return nil, mapErr(err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.AgentRun, error) {
		var (
			run       domain.AgentRun
			tool      *string
			args, res []byte
		)
		err := row.Scan(&run.ID, &run.JobID, &run.Turn, &run.Model, &tool, &args, &res, &run.Outcome,
			&run.PromptTokens, &run.CompletionTokens, &run.LatencyMS, &run.CreatedAt)
		if tool != nil {
			run.ToolName = *tool
		}
		run.ToolArgs, run.ToolResult = rawOrNil(args), rawOrNil(res)
		run.CreatedAt = run.CreatedAt.UTC()
		return run, err
	})
	if runs == nil {
		runs = []domain.AgentRun{}
	}
	return runs, mapErr(err)
}

func (r *AgentJobRepo) one(ctx context.Context, sql string, args ...any) (*domain.AgentJob, error) {
	jobs, err := r.query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, domain.ErrNotFound
	}
	return jobs[0], nil
}

func (r *AgentJobRepo) query(ctx context.Context, sql string, args ...any) ([]*domain.AgentJob, error) {
	rows, err := r.db.q(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*domain.AgentJob, error) {
		var (
			j                       domain.AgentJob
			status                  string
			input, proposed, result []byte
			errMsg, decidedBy       *string
		)
		err := row.Scan(&j.ID, &j.TenantID, &j.TaskID, &j.AgentType, &status, &j.DedupKey, &j.BaseTaskVersion,
			&input, &proposed, &result, &errMsg, &j.Attempts, &decidedBy, &j.DecidedAt, &j.CreatedAt, &j.UpdatedAt,
			&j.RetryAfter, &j.ConflictCount)
		if err != nil {
			return nil, err
		}
		j.Status = domain.JobStatus(status)
		j.InputContext, j.ProposedActions, j.ExecutionResult = rawOrNil(input), rawOrNil(proposed), rawOrNil(result)
		if errMsg != nil {
			j.ErrorMessage = *errMsg
		}
		if decidedBy != nil {
			j.DecidedBy = *decidedBy
		}
		j.CreatedAt, j.UpdatedAt, j.DecidedAt = j.CreatedAt.UTC(), j.UpdatedAt.UTC(), utc(j.DecidedAt)
		j.RetryAfter = utc(j.RetryAfter)
		return &j, nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return jobs, mapErr(err)
}

// jsonObject is raw JSON for a NOT NULL column, defaulting to {}.
func jsonObject(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte(`{}`)
	}
	return raw
}

// jsonOrNull is raw JSON for a nullable column; empty means NULL.
func jsonOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func rawOrNil(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}
