package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// NotificationAckRepo implements repository.NotificationAckRepository.
type NotificationAckRepo struct{ db *DB }

// NewNotificationAckRepo returns a notification ack repository on db.
func NewNotificationAckRepo(db *DB) *NotificationAckRepo { return &NotificationAckRepo{db: db} }

var _ repository.NotificationAckRepository = (*NotificationAckRepo)(nil)

// Save upserts in one statement. A concurrent save of the same key waits for
// the first to commit, then finds the stored reaction and, being the same,
// updates nothing: its RowsAffected is 0, so only one caller sees a change.
func (r *NotificationAckRepo) Save(ctx context.Context, a domain.NotificationAck) (bool, error) {
	var duration *string
	if a.SnoozeDuration != "" {
		duration = &a.SnoozeDuration
	}
	tag, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO notification_acks (tenant_id, user_id, notification_id, signal, snooze_duration, snooze_until, acked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, user_id, notification_id) DO UPDATE
		SET signal = EXCLUDED.signal, snooze_duration = EXCLUDED.snooze_duration,
		    snooze_until = EXCLUDED.snooze_until, acked_at = EXCLUDED.acked_at
		WHERE ROW(notification_acks.signal, notification_acks.snooze_duration)
		      IS DISTINCT FROM ROW(EXCLUDED.signal, EXCLUDED.snooze_duration)`,
		a.TenantID, a.UserID, a.NotificationID, a.Signal, duration, a.SnoozeUntil, a.AckedAt)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *NotificationAckRepo) ListFor(ctx context.Context, tenantID, userID string, ids []string) (map[string]domain.NotificationAck, error) {
	out := map[string]domain.NotificationAck{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT notification_id, signal, COALESCE(snooze_duration, ''), snooze_until, acked_at
		FROM notification_acks
		WHERE tenant_id = $1 AND user_id = $2 AND notification_id = ANY($3)`, tenantID, userID, ids)
	if err != nil {
		return nil, mapErr(err)
	}
	acks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.NotificationAck, error) {
		a := domain.NotificationAck{TenantID: tenantID, UserID: userID}
		err := row.Scan(&a.NotificationID, &a.Signal, &a.SnoozeDuration, &a.SnoozeUntil, &a.AckedAt)
		a.AckedAt = a.AckedAt.UTC()
		if a.SnoozeUntil != nil {
			u := a.SnoozeUntil.UTC()
			a.SnoozeUntil = &u
		}
		return a, err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	for _, a := range acks {
		out[a.NotificationID] = a
	}
	return out, nil
}
