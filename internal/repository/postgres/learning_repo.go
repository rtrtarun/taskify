package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// LearningRepo implements repository.LearningSuggestionRepository.
type LearningRepo struct{ db *DB }

// NewLearningRepo returns a learning suggestion repository on db.
func NewLearningRepo(db *DB) *LearningRepo { return &LearningRepo{db: db} }

var _ repository.LearningSuggestionRepository = (*LearningRepo)(nil)

// Insert relies on the unique index on job, event type, and label: a second
// suggestion of one job for one pattern inserts nothing.
func (r *LearningRepo) Insert(ctx context.Context, s domain.LearningSuggestion) (bool, error) {
	tag, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO learning_suggestions (id, tenant_id, job_id, event_type, label, current_adjustment,
			proposed_adjustment, signal_count, rationale, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT DO NOTHING`,
		s.ID, s.TenantID, s.JobID, s.Pattern.EventType, s.Pattern.Label, s.CurrentAdjustment,
		s.ProposedAdjustment, s.SignalCount, s.Rationale, s.CreatedAt.UTC())
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *LearningRepo) PatternsSince(ctx context.Context, tenantID string, since time.Time) ([]domain.EventPattern, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT DISTINCT event_type, label FROM learning_suggestions
		WHERE tenant_id = $1 AND created_at >= $2
		ORDER BY event_type, label NULLS FIRST`, tenantID, since.UTC())
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.EventPattern, error) {
		var p domain.EventPattern
		err := row.Scan(&p.EventType, &p.Label)
		return p, err
	})
	return out, mapErr(err)
}
