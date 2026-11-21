package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// DependencyRepo implements repository.DependencyRepository.
type DependencyRepo struct{ db *DB }

// NewDependencyRepo returns a dependency repository on db.
func NewDependencyRepo(db *DB) *DependencyRepo { return &DependencyRepo{db: db} }

var _ repository.DependencyRepository = (*DependencyRepo)(nil)

// LockTenant takes a transaction-scoped advisory lock keyed on the tenant.
func (r *DependencyRepo) LockTenant(ctx context.Context, tenantID string) error {
	tx, err := r.db.tx(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('taskify:deps:' || $1, 0))`, tenantID)
	return err
}

func (r *DependencyRepo) Create(ctx context.Context, d domain.Dependency) (bool, error) {
	// ON CONFLICT rather than catching the unique violation: an error would
	// abort the surrounding transaction.
	tag, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO task_dependencies (task_id, depends_on_task_id, tenant_id, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (task_id, depends_on_task_id) DO NOTHING`, d.TaskID, d.DependsOnTaskID, d.TenantID, d.CreatedAt)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// Reaches walks the graph from from. UNION, not UNION ALL, makes the walk
// stop even if the stored graph already contains a cycle.
func (r *DependencyRepo) Reaches(ctx context.Context, tenantID string, from, to uuid.UUID) (bool, error) {
	var found bool
	err := r.db.q(ctx).QueryRow(ctx, `
		WITH RECURSIVE reach (id) AS (
			SELECT depends_on_task_id FROM task_dependencies
			WHERE tenant_id = $1 AND task_id = $2
			UNION
			SELECT td.depends_on_task_id FROM task_dependencies td
			JOIN reach ON td.task_id = reach.id
			WHERE td.tenant_id = $1
		)
		SELECT EXISTS (SELECT 1 FROM reach WHERE id = $3)`, tenantID, from, to).Scan(&found)
	return found, err
}

func (r *DependencyRepo) ListBlockers(ctx context.Context, tenantID string, taskID uuid.UUID) ([]uuid.UUID, error) {
	return r.ids(ctx, `SELECT depends_on_task_id FROM task_dependencies
		WHERE tenant_id = $1 AND task_id = $2 ORDER BY depends_on_task_id`, tenantID, taskID)
}

func (r *DependencyRepo) ListDependents(ctx context.Context, tenantID string, blockerID uuid.UUID) ([]uuid.UUID, error) {
	return r.ids(ctx, `SELECT task_id FROM task_dependencies
		WHERE tenant_id = $1 AND depends_on_task_id = $2 ORDER BY task_id`, tenantID, blockerID)
}

// HasOpenBlocker is the query in section 4.3.
func (r *DependencyRepo) HasOpenBlocker(ctx context.Context, tenantID string, taskID uuid.UUID) (bool, error) {
	var open bool
	err := r.db.q(ctx).QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM task_dependencies td
			JOIN tasks t ON td.depends_on_task_id = t.id
			WHERE td.task_id = $2 AND td.tenant_id = $1
			  AND t.status NOT IN ('completed', 'archived')
		)`, tenantID, taskID).Scan(&open)
	return open, err
}

func (r *DependencyRepo) ids(ctx context.Context, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := r.db.q(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if out == nil {
		out = []uuid.UUID{}
	}
	return out, err
}
