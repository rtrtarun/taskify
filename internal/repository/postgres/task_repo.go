package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// TaskRepo implements repository.TaskRepository.
type TaskRepo struct{ db *DB }

// NewTaskRepo returns a task repository on db.
func NewTaskRepo(db *DB) *TaskRepo { return &TaskRepo{db: db} }

var _ repository.TaskRepository = (*TaskRepo)(nil)

const taskColumns = `id, tenant_id, workspace_id, title, description, status::text, priority::text,
	creator_id, assignee_id, labels, metadata, due_at, remind_at, completed_at, archived_at,
	last_due_soon_notified_at, last_overdue_notified_at, status_before_blocked::text, blocked_reason,
	version, created_at, updated_at, parent_id, estimated_minutes, agent_metadata, unblocked_at`

// openTasks is the predicate of the partial index idx_tasks_due_scan.
const openTasks = `status NOT IN ('completed', 'archived')`

func (r *TaskRepo) Create(ctx context.Context, t *domain.Task) error {
	return r.db.WithTx(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)
		_, err := q.Exec(ctx, `
			INSERT INTO tasks (id, tenant_id, workspace_id, title, description, status, priority,
				creator_id, assignee_id, labels, metadata, due_at, remind_at, completed_at, archived_at,
				last_due_soon_notified_at, last_overdue_notified_at, status_before_blocked, blocked_reason,
				version, created_at, updated_at, parent_id, estimated_minutes, agent_metadata, unblocked_at)
			VALUES ($1, $2, $3, $4, $5, $6::text::task_status, $7::text::task_priority,
				$8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18::text::task_status, $19, $20, $21, $22, $23, $24, $25, $26)`,
			t.ID, t.TenantID, t.WorkspaceID, t.Title, t.Description, string(t.Status), string(t.Priority),
			t.CreatorID, nullString(t.AssigneeID), labels(t.Labels), metadata(t.Metadata),
			t.DueAt, t.RemindAt, t.CompletedAt, t.ArchivedAt,
			t.LastDueSoonNotifiedAt, t.LastOverdueNotifiedAt, statusPtr(t.StatusBeforeBlocked), reasonPtr(t.BlockedReason),
			t.Version, t.CreatedAt, t.UpdatedAt, t.ParentID, t.EstimatedMinutes, agentMetadata(t.AgentMetadata), t.UnblockedAt)
		if err != nil {
			return mapErr(err)
		}
		// A new task has no watchers to remove: insert only.
		return r.insertWatchers(ctx, t.ID, t.Watchers)
	})
}

func (r *TaskRepo) Get(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Task, error) {
	return r.getOne(ctx, tenantID, id, "")
}

func (r *TaskRepo) GetForUpdate(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Task, error) {
	if _, err := r.db.tx(ctx); err != nil {
		return nil, err
	}
	return r.getOne(ctx, tenantID, id, " FOR UPDATE")
}

func (r *TaskRepo) getOne(ctx context.Context, tenantID string, id uuid.UUID, lock string) (*domain.Task, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = $1 AND tenant_id = $2`+lock, id, tenantID)
	t, err := scanTask(row)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := r.loadWatchers(ctx, []*domain.Task{t}); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *TaskRepo) Update(ctx context.Context, t *domain.Task, expectedVersion int) error {
	return r.db.WithTx(ctx, func(ctx context.Context) error {
		tag, err := r.db.q(ctx).Exec(ctx, `
			UPDATE tasks SET
				title = $3, description = $4, status = $5::text::task_status, priority = $6::text::task_priority,
				assignee_id = $7, labels = $8, metadata = $9, due_at = $10, remind_at = $11,
				completed_at = $12, archived_at = $13, last_due_soon_notified_at = $14,
				last_overdue_notified_at = $15, status_before_blocked = $16::text::task_status,
				blocked_reason = $17, version = $18, updated_at = $19,
				estimated_minutes = $21, agent_metadata = $22, unblocked_at = $23
			WHERE id = $1 AND tenant_id = $2 AND version = $20`,
			t.ID, t.TenantID, t.Title, t.Description, string(t.Status), string(t.Priority),
			nullString(t.AssigneeID), labels(t.Labels), metadata(t.Metadata), t.DueAt, t.RemindAt,
			t.CompletedAt, t.ArchivedAt, t.LastDueSoonNotifiedAt,
			t.LastOverdueNotifiedAt, statusPtr(t.StatusBeforeBlocked),
			reasonPtr(t.BlockedReason), t.Version, t.UpdatedAt, expectedVersion,
			t.EstimatedMinutes, agentMetadata(t.AgentMetadata), t.UnblockedAt)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() == 0 {
			// Tell a stale version apart from a missing task.
			var exists bool
			err := r.db.q(ctx).QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1 AND tenant_id = $2)`, t.ID, t.TenantID).Scan(&exists)
			if err != nil {
				return err
			}
			if exists {
				return domain.ErrVersionConflict
			}
			return domain.ErrNotFound
		}
		return r.replaceWatchers(ctx, t.ID, t.Watchers)
	})
}

func (r *TaskRepo) List(ctx context.Context, f repository.TaskFilter) (repository.TaskPage, error) {
	if f.Limit <= 0 {
		return repository.TaskPage{}, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	q, err := newTaskQuery(f, true, false)
	if err != nil {
		return repository.TaskPage{}, err
	}
	sql := `SELECT ` + taskColumns + ` FROM tasks WHERE ` + q.where() +
		` ORDER BY updated_at DESC, id DESC LIMIT ` + q.arg(f.Limit+1)

	tasks, err := r.query(ctx, sql, q.args...)
	if err != nil {
		return repository.TaskPage{}, err
	}
	page := repository.TaskPage{Tasks: tasks}
	if len(tasks) > f.Limit {
		page.Tasks = tasks[:f.Limit]
		last := page.Tasks[f.Limit-1]
		page.Next = &repository.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	}
	return page, nil
}

func (r *TaskRepo) Delete(ctx context.Context, tenantID string, id uuid.UUID) error {
	// Watchers and comments go with the task through ON DELETE CASCADE.
	tag, err := r.db.q(ctx).Exec(ctx, `DELETE FROM tasks WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *TaskRepo) ClaimDueSoon(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error) {
	if _, err := r.db.tx(ctx); err != nil {
		return nil, err
	}
	// The 24h reminder fires once; the 1h reminder fires if nothing was sent
	// since the 1h window opened. See docs/implementation/README.md, G2.
	return r.query(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE `+openTasks+` AND due_at IS NOT NULL
		  AND due_at > $1 AND due_at <= $1 + interval '24 hours'
		  AND (last_due_soon_notified_at IS NULL
		       OR (due_at <= $1 + interval '1 hour'
		           AND last_due_soon_notified_at < due_at - interval '1 hour'))
		ORDER BY due_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, now, limit)
}

func (r *TaskRepo) ClaimOverdue(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error) {
	if _, err := r.db.tx(ctx); err != nil {
		return nil, err
	}
	startOfDay := now.UTC().Truncate(24 * time.Hour)
	return r.query(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE `+openTasks+` AND due_at IS NOT NULL AND due_at <= $1
		  AND (last_overdue_notified_at IS NULL OR last_overdue_notified_at < $2)
		ORDER BY due_at
		LIMIT $3
		FOR UPDATE SKIP LOCKED`, now, startOfDay, limit)
}

func (r *TaskRepo) ClaimStale(ctx context.Context, staleBefore, now time.Time, limit int) ([]*domain.Task, error) {
	if _, err := r.db.tx(ctx); err != nil {
		return nil, err
	}
	// The job key is built as domain.StaleJobDedupKey builds it, so a task
	// already investigated today is not claimed again; the unique dedup key
	// still stops a second job should two scans overlap.
	return r.query(ctx, `
		SELECT `+taskColumns+` FROM tasks t
		WHERE t.status = 'in_progress' AND t.updated_at <= $1
		  AND EXISTS (SELECT 1 FROM workspaces w
		              WHERE w.tenant_id = t.tenant_id AND w.id = t.workspace_id
		                AND COALESCE((w.agent_settings->>'agents_enabled')::boolean, false)
		                AND NOT COALESCE(w.agent_settings->'disabled_agents' ? $2::text, false))
		  AND NOT EXISTS (SELECT 1 FROM task_comments c WHERE c.task_id = t.id AND c.created_at > $1)
		  AND NOT EXISTS (SELECT 1 FROM agent_jobs j
		                  WHERE j.dedup_key = $2::text || ':' || t.id::text || ':stale:' || $3::text)
		ORDER BY t.updated_at, t.id
		LIMIT $4
		FOR UPDATE OF t SKIP LOCKED`,
		staleBefore, domain.AgentBlocker, now.UTC().Format("2006-01-02"), limit)
}

func (r *TaskRepo) ListBriefRecipients(ctx context.Context, day time.Time, limit int) ([]domain.BriefRecipient, error) {
	// The job key is built as domain.BriefJobDedupKey builds it. Byte order
	// (COLLATE "C") keeps the order and the chosen workspace independent of
	// the database's locale.
	rows, err := r.db.q(ctx).Query(ctx, `
		WITH candidates AS (
			SELECT tenant_id, assignee_id AS user_id, workspace_id FROM tasks
			WHERE status IN ('todo', 'in_progress') AND assignee_id IS NOT NULL
			UNION
			SELECT t.tenant_id, COALESCE(t.assignee_id, t.creator_id), t.workspace_id
			FROM agent_jobs j JOIN tasks t ON t.id = j.task_id
			WHERE j.status = 'awaiting_approval'
		)
		SELECT c.tenant_id, c.user_id, min(c.workspace_id COLLATE "C") FROM candidates c
		JOIN workspaces w ON w.tenant_id = c.tenant_id AND w.id = c.workspace_id
		WHERE COALESCE((w.agent_settings->>'agents_enabled')::boolean, false)
		  AND NOT COALESCE(w.agent_settings->'disabled_agents' ? $1::text, false)
		  AND NOT EXISTS (SELECT 1 FROM agent_jobs d
		                  WHERE d.dedup_key = $1::text || ':' || c.tenant_id || ':' || c.user_id || ':brief:' || $2::text)
		GROUP BY c.tenant_id, c.user_id
		ORDER BY c.tenant_id COLLATE "C", c.user_id COLLATE "C"
		LIMIT $3`, domain.AgentDigest, day.UTC().Format(time.DateOnly), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.BriefRecipient, error) {
		var r domain.BriefRecipient
		err := row.Scan(&r.TenantID, &r.UserID, &r.WorkspaceID)
		return r, err
	})
	return out, mapErr(err)
}

func (r *TaskRepo) MarkDueSoonNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	return r.mark(ctx, "last_due_soon_notified_at", tenantID, id, at)
}

func (r *TaskRepo) MarkOverdueNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	return r.mark(ctx, "last_overdue_notified_at", tenantID, id, at)
}

func (r *TaskRepo) mark(ctx context.Context, column, tenantID string, id uuid.UUID, at time.Time) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE tasks SET `+column+` = $3 WHERE id = $1 AND tenant_id = $2`, id, tenantID, at)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// query runs a task SELECT and loads watchers for the result.
func (r *TaskRepo) query(ctx context.Context, sql string, args ...any) ([]*domain.Task, error) {
	rows, err := r.db.q(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*domain.Task, error) { return scanTask(row) })
	if err != nil {
		return nil, mapErr(err)
	}
	if err := r.loadWatchers(ctx, tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (r *TaskRepo) loadWatchers(ctx context.Context, tasks []*domain.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	ids := make([]string, len(tasks))
	byID := make(map[uuid.UUID]*domain.Task, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID.String()
		t.Watchers = []string{}
		byID[t.ID] = t
	}
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT task_id, user_id FROM task_watchers WHERE task_id = ANY($1::uuid[]) ORDER BY user_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID uuid.UUID
		var user string
		if err := rows.Scan(&taskID, &user); err != nil {
			return err
		}
		byID[taskID].Watchers = append(byID[taskID].Watchers, user)
	}
	return rows.Err()
}

func (r *TaskRepo) replaceWatchers(ctx context.Context, taskID uuid.UUID, watchers []string) error {
	q := r.db.q(ctx)
	if watchers == nil {
		watchers = []string{}
	}
	if _, err := q.Exec(ctx,
		`DELETE FROM task_watchers WHERE task_id = $1 AND NOT (user_id = ANY($2::text[]))`, taskID, watchers); err != nil {
		return mapErr(err)
	}
	return r.insertWatchers(ctx, taskID, watchers)
}

func (r *TaskRepo) insertWatchers(ctx context.Context, taskID uuid.UUID, watchers []string) error {
	if len(watchers) == 0 {
		return nil
	}
	_, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO task_watchers (task_id, user_id)
		SELECT $1, w FROM unnest($2::text[]) AS w
		ON CONFLICT DO NOTHING`, taskID, watchers)
	return mapErr(err)
}

func scanTask(row pgx.Row) (*domain.Task, error) {
	var (
		t                         domain.Task
		status, priority          string
		assignee, before, reason  *string
		due, remind, completed    *time.Time
		archived, dueSoon, overdu *time.Time
		unblocked                 *time.Time
		agentMeta                 []byte
	)
	err := row.Scan(&t.ID, &t.TenantID, &t.WorkspaceID, &t.Title, &t.Description, &status, &priority,
		&t.CreatorID, &assignee, &t.Labels, &t.Metadata, &due, &remind, &completed, &archived,
		&dueSoon, &overdu, &before, &reason, &t.Version, &t.CreatedAt, &t.UpdatedAt, &t.ParentID,
		&t.EstimatedMinutes, &agentMeta, &unblocked)
	if err != nil {
		return nil, err
	}
	if t.AgentMetadata, err = domain.ParseAgentMetadata(agentMeta); err != nil {
		return nil, fmt.Errorf("task %s: stored %w", t.ID, err)
	}
	t.Status = domain.Status(status)
	t.Priority = domain.Priority(priority)
	if assignee != nil {
		t.AssigneeID = *assignee
	}
	if before != nil {
		s := domain.Status(*before)
		t.StatusBeforeBlocked = &s
	}
	if reason != nil {
		br := domain.BlockedReason(*reason)
		t.BlockedReason = &br
	}
	t.DueAt, t.RemindAt, t.CompletedAt, t.ArchivedAt = utc(due), utc(remind), utc(completed), utc(archived)
	t.LastDueSoonNotifiedAt, t.LastOverdueNotifiedAt = utc(dueSoon), utc(overdu)
	t.UnblockedAt = utc(unblocked)
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if t.Metadata == nil {
		t.Metadata = map[string]any{}
	}
	return &t, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func labels(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// agentMetadata is the JSON stored in tasks.agent_metadata.
func agentMetadata(m domain.AgentMetadata) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{}`) // a struct of plain fields always encodes
	}
	return b
}

func metadata(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func statusPtr(s *domain.Status) *string {
	if s == nil {
		return nil
	}
	v := string(*s)
	return &v
}

func reasonPtr(r *domain.BlockedReason) *string {
	if r == nil {
		return nil
	}
	v := string(*r)
	return &v
}

// CountAssigned is served by idx_tasks_tenant_assignee_status.
func (r *TaskRepo) CountAssigned(ctx context.Context, tenantID, assigneeID string, status domain.Status, priority domain.Priority) (int, error) {
	var n int
	err := r.db.q(ctx).QueryRow(ctx, `
		SELECT count(*) FROM tasks
		WHERE tenant_id = $1 AND assignee_id = $2 AND status = $3::text::task_status AND priority = $4::text::task_priority`,
		tenantID, assigneeID, string(status), string(priority)).Scan(&n)
	return n, mapErr(err)
}

func (r *TaskRepo) Existing(ctx context.Context, tenantID string, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return []uuid.UUID{}, nil
	}
	rows, err := r.db.q(ctx).Query(ctx, `SELECT id FROM tasks WHERE tenant_id = $1 AND id = ANY($2) ORDER BY id`, tenantID, ids)
	if err != nil {
		return nil, mapErr(err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return found, mapErr(err)
}

func (r *TaskRepo) WorkspaceLabels(ctx context.Context, tenantID, workspaceID string, limit int) ([]string, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT l FROM tasks, unnest(labels) AS l
		WHERE tenant_id = $1 AND workspace_id = $2
		GROUP BY l ORDER BY count(*) DESC, l LIMIT $3`, tenantID, workspaceID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, mapErr(err)
}

// SimilarOpen ranks titles with English full-text search: any shared word
// (after stemming) is a match. The query is built from domain.SearchTerms,
// plain letters and digits, so it needs no escaping.
func (r *TaskRepo) SimilarOpen(ctx context.Context, tenantID, workspaceID string, exclude uuid.UUID, text string, limit int) ([]*domain.Task, error) {
	terms := domain.SearchTerms(text)
	if len(terms) == 0 {
		return []*domain.Task{}, nil
	}
	tasks, err := r.query(ctx, `
		SELECT `+taskColumns+` FROM tasks, to_tsquery('english', $4) AS q
		WHERE tenant_id = $1 AND workspace_id = $2 AND id <> $3 AND `+openTasks+`
		  AND to_tsvector('english', title) @@ q
		ORDER BY ts_rank(to_tsvector('english', title), q) DESC, updated_at DESC, id
		LIMIT $5`, tenantID, workspaceID, exclude, strings.Join(terms, " | "), limit)
	if tasks == nil {
		tasks = []*domain.Task{}
	}
	return tasks, err
}

// taskQuery builds the WHERE clause of a task list.
type taskQuery struct {
	args  []any
	conds []string
}

func (q *taskQuery) arg(v any) string {
	q.args = append(q.args, v)
	return fmt.Sprintf("$%d", len(q.args))
}

func (q *taskQuery) where() string { return strings.Join(q.conds, " AND ") }

// newTaskQuery turns a filter into conditions. withCursor adds the page
// cursor; archivedOnly replaces the status condition with "archived since
// UpdatedSince", ignoring f.Status.
func newTaskQuery(f repository.TaskFilter, withCursor, archivedOnly bool) (*taskQuery, error) {
	q := &taskQuery{args: []any{f.TenantID}, conds: []string{"tenant_id = $1"}}
	switch {
	case archivedOnly:
		q.conds = append(q.conds, "status = 'archived'", "archived_at > "+q.arg(f.UpdatedSince)+"::timestamptz")
	case f.Status != "":
		q.conds = append(q.conds, "status = "+q.arg(string(f.Status))+"::text::task_status")
	default:
		q.conds = append(q.conds, "status <> 'archived'")
	}
	if f.AssigneeID != "" {
		q.conds = append(q.conds, "assignee_id = "+q.arg(f.AssigneeID))
	}
	if f.Priority != "" {
		q.conds = append(q.conds, "priority = "+q.arg(string(f.Priority))+"::text::task_priority")
	}
	if f.IDPrefix != "" {
		// A range on the primary key: UUIDs order as their canonical text.
		lo, hi, err := domain.IDPrefixRange(f.IDPrefix)
		if err != nil {
			return nil, err
		}
		q.conds = append(q.conds, "id BETWEEN "+q.arg(lo)+"::uuid AND "+q.arg(hi)+"::uuid")
	}
	if f.UpdatedSince != nil && !archivedOnly {
		q.conds = append(q.conds, "updated_at > "+q.arg(*f.UpdatedSince)+"::timestamptz")
	}
	if withCursor && f.After != nil {
		q.conds = append(q.conds, fmt.Sprintf("(updated_at, id) < (%s::timestamptz, %s::uuid)", q.arg(f.After.UpdatedAt), q.arg(f.After.ID)))
	}
	return q, nil
}

func (r *TaskRepo) ListState(ctx context.Context, f repository.TaskFilter) (repository.ListState, error) {
	q, err := newTaskQuery(f, false, false)
	if err != nil {
		return repository.ListState{}, err
	}
	var st repository.ListState
	var latest *time.Time
	if err := r.db.q(ctx).QueryRow(ctx,
		`SELECT count(*), max(updated_at) FROM tasks WHERE `+q.where(), q.args...).Scan(&st.Count, &latest); err != nil {
		return repository.ListState{}, mapErr(err)
	}
	if latest != nil {
		st.Latest = latest.UTC()
	}
	if f.UpdatedSince != nil {
		aq, err := newTaskQuery(f, false, true)
		if err != nil {
			return repository.ListState{}, err
		}
		if err := r.db.q(ctx).QueryRow(ctx,
			`SELECT count(*) FROM tasks WHERE `+aq.where(), aq.args...).Scan(&st.ArchivedSince); err != nil {
			return repository.ListState{}, mapErr(err)
		}
	}
	return st, nil
}

func (r *TaskRepo) ArchivedSince(ctx context.Context, f repository.TaskFilter) ([]uuid.UUID, error) {
	if f.UpdatedSince == nil {
		return []uuid.UUID{}, nil
	}
	q, err := newTaskQuery(f, false, true)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT id FROM tasks WHERE `+q.where()+` ORDER BY archived_at, id LIMIT `+q.arg(repository.MaxArchivedIDs), q.args...)
	if err != nil {
		return nil, mapErr(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, mapErr(err)
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return ids, nil
}
