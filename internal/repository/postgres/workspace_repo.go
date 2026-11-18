package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RONGALI-TARUN/taskify/internal/domain"
	"github.com/RONGALI-TARUN/taskify/internal/repository"
)

// WorkspaceRepo implements repository.WorkspaceRepository.
type WorkspaceRepo struct{ db *DB }

// NewWorkspaceRepo returns a workspace repository on db.
func NewWorkspaceRepo(db *DB) *WorkspaceRepo { return &WorkspaceRepo{db: db} }

var _ repository.WorkspaceRepository = (*WorkspaceRepo)(nil)

func (r *WorkspaceRepo) Create(ctx context.Context, w domain.Workspace) error {
	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO workspaces (tenant_id, id, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		w.TenantID, w.ID, w.Name, w.CreatedAt, w.UpdatedAt)
	return mapErr(err)
}

func (r *WorkspaceRepo) Get(ctx context.Context, tenantID, id string) (domain.Workspace, error) {
	return r.get(ctx, tenantID, id, "")
}

func (r *WorkspaceRepo) GetForUpdate(ctx context.Context, tenantID, id string) (domain.Workspace, error) {
	if _, err := r.db.tx(ctx); err != nil {
		return domain.Workspace{}, err
	}
	return r.get(ctx, tenantID, id, " FOR UPDATE")
}

func (r *WorkspaceRepo) Update(ctx context.Context, w domain.Workspace) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE workspaces SET name = $3, updated_at = $4 WHERE tenant_id = $1 AND id = $2`,
		w.TenantID, w.ID, w.Name, w.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *WorkspaceRepo) get(ctx context.Context, tenantID, id, lock string) (domain.Workspace, error) {
	var w domain.Workspace
	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT tenant_id, id, name, created_at, updated_at FROM workspaces WHERE tenant_id = $1 AND id = $2`+lock,
		tenantID, id).Scan(&w.TenantID, &w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return domain.Workspace{}, mapErr(err)
	}
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	return w, nil
}

// CommentRepo implements repository.CommentRepository.
type CommentRepo struct{ db *DB }

// NewCommentRepo returns a comment repository on db.
func NewCommentRepo(db *DB) *CommentRepo { return &CommentRepo{db: db} }

var _ repository.CommentRepository = (*CommentRepo)(nil)

func (r *CommentRepo) Create(ctx context.Context, c domain.Comment) error {
	mentions := c.Mentions
	if mentions == nil {
		mentions = []string{}
	}
	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO task_comments (id, task_id, author_id, body, mentions, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		c.ID, c.TaskID, c.AuthorID, c.Body, mentions, c.CreatedAt)
	return mapErr(err)
}

func (r *CommentRepo) ListRecent(ctx context.Context, taskID uuid.UUID, limit int) ([]domain.Comment, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT id, task_id, author_id, body, mentions, created_at FROM task_comments
		WHERE task_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Comment, error) {
		var c domain.Comment
		err := row.Scan(&c.ID, &c.TaskID, &c.AuthorID, &c.Body, &c.Mentions, &c.CreatedAt)
		c.CreatedAt = c.CreatedAt.UTC()
		if c.Mentions == nil {
			c.Mentions = []string{}
		}
		return c, err
	})
}
