package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RONGALI-TARUN/taskify/internal/domain"
	"github.com/RONGALI-TARUN/taskify/internal/repository"
)

// RuleRepo implements repository.WorkspaceRuleRepository.
type RuleRepo struct{ db *DB }

// NewRuleRepo returns a workspace rule repository on db.
func NewRuleRepo(db *DB) *RuleRepo { return &RuleRepo{db: db} }

var _ repository.WorkspaceRuleRepository = (*RuleRepo)(nil)

func (r *RuleRepo) List(ctx context.Context, tenantID, workspaceID string) ([]domain.WorkspaceRule, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT tenant_id, workspace_id, rule_key, agentpulse_rule_id, created_at FROM workspace_rules
		WHERE tenant_id = $1 AND workspace_id = $2 ORDER BY rule_key`, tenantID, workspaceID)
	if err != nil {
		return nil, mapErr(err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.WorkspaceRule, error) {
		var w domain.WorkspaceRule
		err := row.Scan(&w.TenantID, &w.WorkspaceID, &w.Key, &w.AgentPulseRuleID, &w.CreatedAt)
		w.CreatedAt = w.CreatedAt.UTC()
		return w, err
	})
}

func (r *RuleRepo) Create(ctx context.Context, w domain.WorkspaceRule) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO workspace_rules (tenant_id, workspace_id, rule_key, agentpulse_rule_id, created_at)
		VALUES ($1, $2, $3, $4, $5)`, w.TenantID, w.WorkspaceID, w.Key, w.AgentPulseRuleID, w.CreatedAt)
	return mapErr(err)
}

// DeliveryRepo implements repository.WebhookDeliveryRepository.
type DeliveryRepo struct{ db *DB }

// NewDeliveryRepo returns a webhook delivery repository on db.
func NewDeliveryRepo(db *DB) *DeliveryRepo { return &DeliveryRepo{db: db} }

var _ repository.WebhookDeliveryRepository = (*DeliveryRepo)(nil)

func (r *DeliveryRepo) Record(ctx context.Context, key string, at time.Time) (bool, error) {
	tag, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO webhook_deliveries (signature, received_at) VALUES ($1, $2)
		ON CONFLICT (signature) DO NOTHING`, key, at)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *DeliveryRepo) DeleteBefore(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := r.db.q(ctx).Exec(ctx, `DELETE FROM webhook_deliveries WHERE received_at < $1`, cutoff)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}
