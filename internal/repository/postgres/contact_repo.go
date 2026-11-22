package postgres

import (
	"context"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// ContactRepo implements repository.ContactRepository.
type ContactRepo struct{ db *DB }

// NewContactRepo returns a contact repository on db.
func NewContactRepo(db *DB) *ContactRepo { return &ContactRepo{db: db} }

var _ repository.ContactRepository = (*ContactRepo)(nil)

const contactColumns = `tenant_id, user_id, email, slack_user_id, phone, updated_at`

func (r *ContactRepo) Get(ctx context.Context, tenantID, userID string) (domain.Contact, error) {
	return r.one(ctx, `SELECT `+contactColumns+` FROM user_contacts WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
}

// GetForUpdate inserts an empty row if there is none, then locks it. Two
// first-time updates of one person thus wait for each other on the row,
// rather than both reading "no contact" and the second overwriting the
// first.
func (r *ContactRepo) GetForUpdate(ctx context.Context, tenantID, userID string) (domain.Contact, error) {
	tx, err := r.db.tx(ctx)
	if err != nil {
		return domain.Contact{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_contacts (tenant_id, user_id) VALUES ($1, $2)
		ON CONFLICT (tenant_id, user_id) DO NOTHING`, tenantID, userID); err != nil {
		return domain.Contact{}, mapErr(err)
	}
	return r.one(ctx, `SELECT `+contactColumns+` FROM user_contacts WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`,
		tenantID, userID)
}

func (r *ContactRepo) Save(ctx context.Context, c domain.Contact) error {
	tag, err := r.db.q(ctx).Exec(ctx, `
		UPDATE user_contacts SET email = $3, slack_user_id = $4, phone = $5, updated_at = $6
		WHERE tenant_id = $1 AND user_id = $2`,
		c.TenantID, c.UserID, c.Email, c.SlackUserID, c.Phone, c.UpdatedAt.UTC().Truncate(time.Microsecond))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *ContactRepo) one(ctx context.Context, sql string, args ...any) (domain.Contact, error) {
	var (
		c       domain.Contact
		updated *time.Time
	)
	err := r.db.q(ctx).QueryRow(ctx, sql, args...).Scan(&c.TenantID, &c.UserID, &c.Email, &c.SlackUserID, &c.Phone, &updated)
	if err != nil {
		return domain.Contact{}, mapErr(err)
	}
	if updated != nil { // NULL for a row created only to be locked
		c.UpdatedAt = updated.UTC()
	}
	return c, nil
}
