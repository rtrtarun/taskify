package memory

import (
	"context"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

type contactKey struct{ tenant, user string }

// ContactRepo implements repository.ContactRepository.
type ContactRepo struct{ st *Store }

// Contacts returns the contact repository.
func (st *Store) Contacts() *ContactRepo { return &ContactRepo{st} }

var _ repository.ContactRepository = (*ContactRepo)(nil)

func (r *ContactRepo) Get(ctx context.Context, tenantID, userID string) (c domain.Contact, err error) {
	err = r.st.do(ctx, func(s *state) error {
		var ok bool
		if c, ok = s.contacts[contactKey{tenantID, userID}]; !ok {
			return domain.ErrNotFound
		}
		return nil
	})
	return c, err
}

// GetForUpdate needs no lock of its own: the transaction already holds the
// store's.
func (r *ContactRepo) GetForUpdate(ctx context.Context, tenantID, userID string) (domain.Contact, error) {
	if !inTx(ctx) {
		return domain.Contact{}, repository.ErrTxRequired
	}
	s := &r.st.s
	k := contactKey{tenantID, userID}
	c, ok := s.contacts[k]
	if !ok {
		c = domain.Contact{TenantID: tenantID, UserID: userID}
		s.contacts[k] = c
	}
	return c, nil
}

func (r *ContactRepo) Save(ctx context.Context, c domain.Contact) error {
	return r.st.do(ctx, func(s *state) error {
		k := contactKey{c.TenantID, c.UserID}
		if _, ok := s.contacts[k]; !ok {
			return domain.ErrNotFound
		}
		c.UpdatedAt = micro(c.UpdatedAt)
		c.Email, c.SlackUserID, c.Phone = cloneStr(c.Email), cloneStr(c.SlackUserID), cloneStr(c.Phone)
		s.contacts[k] = c
		return nil
	})
}

func cloneStr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
