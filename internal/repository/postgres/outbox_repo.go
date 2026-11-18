package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RONGALI-TARUN/taskify/internal/domain"
	"github.com/RONGALI-TARUN/taskify/internal/repository"
)

// OutboxRepo implements repository.OutboxRepository.
type OutboxRepo struct{ db *DB }

// NewOutboxRepo returns an outbox repository on db.
func NewOutboxRepo(db *DB) *OutboxRepo { return &OutboxRepo{db: db} }

var _ repository.OutboxRepository = (*OutboxRepo)(nil)

const outboxColumns = `id, tenant_id, aggregate_id, event_type, idempotency_key, payload, trace_context,
	status::text, attempts, max_attempts, next_attempt_at, claimed_at, last_error, created_at, dispatched_at`

const maxErrorLen = 1000

func (r *OutboxRepo) Insert(ctx context.Context, entries []domain.OutboxEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	inserted := 0
	err := r.db.WithTx(ctx, func(ctx context.Context) error {
		batch := &pgx.Batch{}
		for _, e := range entries {
			trace := e.TraceContext
			if trace == nil {
				trace = map[string]string{}
			}
			batch.Queue(`
				INSERT INTO notification_outbox (id, tenant_id, aggregate_id, event_type, idempotency_key,
					payload, trace_context, status, attempts, max_attempts, next_attempt_at, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', 0, $8, $9, $10)
				ON CONFLICT (idempotency_key) DO NOTHING`,
				e.ID, e.TenantID, e.AggregateID, e.EventType, e.IdempotencyKey,
				json.RawMessage(e.Payload), trace, e.MaxAttempts, e.NextAttemptAt, e.CreatedAt)
		}
		tx, err := r.db.tx(ctx)
		if err != nil {
			return err
		}
		res := tx.SendBatch(ctx, batch)
		defer res.Close()
		for range entries {
			tag, err := res.Exec()
			if err != nil {
				return mapErr(err)
			}
			inserted += int(tag.RowsAffected())
		}
		return res.Close()
	})
	return inserted, err
}

func (r *OutboxRepo) ClaimBatch(ctx context.Context, now time.Time, limit int) ([]domain.OutboxEntry, error) {
	// One statement: the subquery locks due rows, skipping those another
	// claimer holds, and the update marks them in the same snapshot.
	claimedAt := now.UTC().Truncate(time.Microsecond)
	return r.query(ctx, `
		UPDATE notification_outbox SET status = 'processing', claimed_at = $1
		WHERE id IN (
			SELECT id FROM notification_outbox
			WHERE status = 'pending' AND next_attempt_at <= $1
			ORDER BY next_attempt_at, created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING `+outboxColumns, claimedAt, limit)
}

// fence matches only the row still held by the given claim.
const fence = `id = $1 AND status = 'processing' AND claimed_at = $2`

func (r *OutboxRepo) MarkDispatched(ctx context.Context, id uuid.UUID, claimedAt, at time.Time) error {
	_, err := r.db.q(ctx).Exec(ctx,
		`UPDATE notification_outbox SET status = 'dispatched', dispatched_at = $3, claimed_at = NULL WHERE `+fence,
		id, claimedAt, at)
	return mapErr(err)
}

func (r *OutboxRepo) MarkRetry(ctx context.Context, id uuid.UUID, claimedAt time.Time, attempts int, next time.Time, lastErr string) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		UPDATE notification_outbox
		SET status = 'pending', attempts = $3, next_attempt_at = $4, last_error = $5, claimed_at = NULL
		WHERE `+fence, id, claimedAt, attempts, next, truncateErr(lastErr))
	return mapErr(err)
}

func (r *OutboxRepo) Release(ctx context.Context, id uuid.UUID, claimedAt, next time.Time) error {
	_, err := r.db.q(ctx).Exec(ctx,
		`UPDATE notification_outbox SET status = 'pending', next_attempt_at = $3, claimed_at = NULL WHERE `+fence,
		id, claimedAt, next)
	return mapErr(err)
}

func (r *OutboxRepo) MarkDeadLetter(ctx context.Context, id uuid.UUID, claimedAt time.Time, attempts int, lastErr string) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		UPDATE notification_outbox
		SET status = 'dead_letter', attempts = $3, last_error = $4, claimed_at = NULL
		WHERE `+fence, id, claimedAt, attempts, truncateErr(lastErr))
	return mapErr(err)
}

func (r *OutboxRepo) ReleaseExpiredLeases(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := r.db.q(ctx).Exec(ctx, `
		UPDATE notification_outbox SET status = 'pending', claimed_at = NULL
		WHERE status = 'processing' AND claimed_at < $1`, olderThan)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *OutboxRepo) DeleteDispatchedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	tag, err := r.db.q(ctx).Exec(ctx, `
		DELETE FROM notification_outbox WHERE id IN (
			SELECT id FROM notification_outbox
			WHERE status = 'dispatched' AND dispatched_at < $1
			LIMIT $2)`, cutoff, limit)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *OutboxRepo) Get(ctx context.Context, id uuid.UUID) (domain.OutboxEntry, error) {
	entries, err := r.query(ctx, `SELECT `+outboxColumns+` FROM notification_outbox WHERE id = $1`, id)
	if err != nil {
		return domain.OutboxEntry{}, err
	}
	if len(entries) == 0 {
		return domain.OutboxEntry{}, domain.ErrNotFound
	}
	return entries[0], nil
}

func (r *OutboxRepo) ListForAggregate(ctx context.Context, aggregateID uuid.UUID) ([]domain.OutboxEntry, error) {
	return r.query(ctx, `SELECT `+outboxColumns+` FROM notification_outbox
		WHERE aggregate_id = $1 ORDER BY created_at, id`, aggregateID)
}

func (r *OutboxRepo) CountByStatus(ctx context.Context, status domain.OutboxStatus) (int, error) {
	var n int
	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT count(*) FROM notification_outbox WHERE status = $1::text::outbox_status`, string(status)).Scan(&n)
	return n, mapErr(err)
}

func (r *OutboxRepo) query(ctx context.Context, sql string, args ...any) ([]domain.OutboxEntry, error) {
	rows, err := r.db.q(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.OutboxEntry, error) {
		var (
			e         domain.OutboxEntry
			status    string
			lastError *string
			payload   []byte
		)
		err := row.Scan(&e.ID, &e.TenantID, &e.AggregateID, &e.EventType, &e.IdempotencyKey, &payload,
			&e.TraceContext, &status, &e.Attempts, &e.MaxAttempts, &e.NextAttemptAt, &e.ClaimedAt,
			&lastError, &e.CreatedAt, &e.DispatchedAt)
		if err != nil {
			return e, err
		}
		e.Payload = payload
		e.Status = domain.OutboxStatus(status)
		if lastError != nil {
			e.LastError = *lastError
		}
		e.NextAttemptAt, e.CreatedAt = e.NextAttemptAt.UTC(), e.CreatedAt.UTC()
		e.ClaimedAt, e.DispatchedAt = utc(e.ClaimedAt), utc(e.DispatchedAt)
		return e, nil
	})
	return entries, mapErr(err)
}

func truncateErr(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	// Cutting bytes can split a character; PostgreSQL rejects invalid UTF-8.
	return strings.ToValidUTF8(s[:maxErrorLen], "")
}
