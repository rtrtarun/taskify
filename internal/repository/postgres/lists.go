package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// The list reads the clients need (M12-04). Pages are ordered by
// (created_at, id) descending and continue after the cursor, which
// migration 000009's indexes serve.

func (r *TaskRepo) ListChildren(ctx context.Context, tenantID string, parentID uuid.UUID) ([]*domain.Task, error) {
	tasks, err := r.query(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE tenant_id = $1 AND parent_id = $2 ORDER BY created_at, id`, tenantID, parentID)
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		tasks = []*domain.Task{}
	}
	return tasks, nil
}

// pageQuery builds the WHERE clause shared by the list reads.
type pageQuery struct {
	args  []any
	where []string
}

func (q *pageQuery) arg(v any) string {
	q.args = append(q.args, v)
	return fmt.Sprintf("$%d", len(q.args))
}

func (q *pageQuery) after(c *repository.ListCursor) {
	if c != nil {
		q.where = append(q.where, fmt.Sprintf("(created_at, id) < (%s::timestamptz, %s::uuid)",
			q.arg(c.CreatedAt.UTC()), q.arg(c.ID)))
	}
}

func (q *pageQuery) sql(table, columns string, limit int) string {
	return `SELECT ` + columns + ` FROM ` + table + ` WHERE ` + strings.Join(q.where, " AND ") +
		` ORDER BY created_at DESC, id DESC LIMIT ` + q.arg(limit+1)
}

func (r *AgentJobRepo) List(ctx context.Context, f repository.AgentJobFilter) (repository.AgentJobPage, error) {
	if f.Limit <= 0 {
		return repository.AgentJobPage{}, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	q := &pageQuery{}
	q.where = append(q.where, "tenant_id = "+q.arg(f.TenantID))
	if f.Status != "" {
		q.where = append(q.where, "status = "+q.arg(string(f.Status))+"::text::agent_job_status")
	}
	if f.TaskID != nil {
		q.where = append(q.where, "task_id = "+q.arg(*f.TaskID))
	}
	if f.AgentType != "" {
		q.where = append(q.where, "agent_type = "+q.arg(f.AgentType))
	}
	if f.Viewer != "" && !f.ViewerIsAdmin {
		// domain.AgentJob.VisibleTo: a job on a task, or one for this person.
		q.where = append(q.where, "(task_id IS NOT NULL OR input_context->>'user_id' = "+q.arg(f.Viewer)+")")
	}
	q.after(f.After)
	jobs, err := r.query(ctx, q.sql("agent_jobs", jobColumns, f.Limit), q.args...)
	if err != nil {
		return repository.AgentJobPage{}, err
	}
	page := repository.AgentJobPage{Jobs: jobs}
	if page.Jobs == nil {
		page.Jobs = []*domain.AgentJob{}
	}
	if len(jobs) > f.Limit {
		page.Jobs = jobs[:f.Limit]
		last := page.Jobs[f.Limit-1]
		page.Next = &repository.ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (r *OutboxRepo) List(ctx context.Context, f repository.OutboxFilter) (repository.OutboxPage, error) {
	if f.Limit <= 0 {
		return repository.OutboxPage{}, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	q := &pageQuery{}
	q.where = append(q.where, "tenant_id = "+q.arg(f.TenantID))
	if f.Status != "" {
		q.where = append(q.where, "status = "+q.arg(string(f.Status))+"::text::outbox_status")
	}
	q.after(f.After)
	entries, err := r.query(ctx, q.sql("notification_outbox", outboxColumns, f.Limit), q.args...)
	if err != nil {
		return repository.OutboxPage{}, err
	}
	page := repository.OutboxPage{Entries: entries}
	if page.Entries == nil {
		page.Entries = []domain.OutboxEntry{}
	}
	if len(entries) > f.Limit {
		page.Entries = entries[:f.Limit]
		last := page.Entries[f.Limit-1]
		page.Next = &repository.ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

// Replay is one statement for the row that qualifies; a second read tells
// a row in another status from no row, for the error.
func (r *OutboxRepo) Replay(ctx context.Context, tenantID string, id uuid.UUID, now time.Time) (domain.OutboxEntry, error) {
	entries, err := r.query(ctx, `
		UPDATE notification_outbox
		SET status = 'pending', attempts = 0, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND tenant_id = $2 AND status = 'dead_letter'
		RETURNING `+outboxColumns, id, tenantID, now.UTC().Truncate(time.Microsecond))
	if err != nil {
		return domain.OutboxEntry{}, err
	}
	if len(entries) == 1 {
		return entries[0], nil
	}
	var exists bool
	err = r.db.q(ctx).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM notification_outbox WHERE id = $1 AND tenant_id = $2)`, id, tenantID).Scan(&exists)
	if err != nil {
		return domain.OutboxEntry{}, mapErr(err)
	}
	if exists {
		return domain.OutboxEntry{}, domain.ErrNotDeadLetter
	}
	return domain.OutboxEntry{}, domain.ErrNotFound
}
