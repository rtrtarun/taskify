package service

import (
	"context"
	"errors"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

// ContactService keeps people's contact details and forwards them to
// AgentPulse (AP-05). Like a task write, it changes PostgreSQL only: the
// contact and its outbox row commit together, and the relay makes the call.
//
// Ordering. AgentPulse replaces a contact whole on every PUT and keeps no
// version, so the order in which PUTs arrive decides what it ends up with.
// Each person has one contact.update outbox row, reused for every change
// (OutboxRepository.Coalesce): an update rewrites the row's payload with the
// whole latest contact. The relay claims a row exclusively, so one person's
// contact is never in flight twice. When a send succeeds, the relay marks the
// row dispatched only if its payload is still the one it sent; if an update
// arrived meanwhile, the row goes back to pending and the newer contact is
// sent next. Every send therefore carries a contact at least as new as the
// previous send's, and the last one carries the latest, whatever the timing
// of updates, retries, and replicas.
type ContactService struct {
	tx          repository.TxManager
	contacts    repository.ContactRepository
	outbox      repository.OutboxRepository
	maxAttempts int
	now         func() time.Time
	wake        func()
}

// NewContactService returns a ContactService. wake, called after a change
// commits, may be nil.
func NewContactService(tx repository.TxManager, contacts repository.ContactRepository, outbox repository.OutboxRepository,
	maxAttempts int, now func() time.Time, wake func()) *ContactService {
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	if now == nil {
		now = time.Now
	}
	if wake == nil {
		wake = func() {}
	}
	return &ContactService{tx: tx, contacts: contacts, outbox: outbox, maxAttempts: maxAttempts, now: now, wake: wake}
}

// Get returns the person's contact; one who never set it has an empty
// contact with a zero UpdatedAt.
func (s *ContactService) Get(ctx context.Context, tenantID, userID string) (domain.Contact, error) {
	c, err := s.contacts.Get(ctx, tenantID, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Contact{TenantID: tenantID, UserID: userID}, nil
	}
	return c, err
}

// errUnchanged rolls back an update that changes nothing, so it leaves no
// row behind and sends nothing.
var errUnchanged = errors.New("contact unchanged")

// Update applies the person's change to their own contact. Callers pass the
// tenant and user of the authenticated caller. A change that leaves the
// contact as it was writes nothing and forwards nothing, as AgentPulse would
// ignore it anyway.
//
// The person's contact row is locked for the transaction, so two updates of
// one person run one after the other, each on the other's result, and their
// outbox writes happen in the same order.
func (s *ContactService) Update(ctx context.Context, tenantID, userID string, p domain.ContactPatch) (domain.Contact, error) {
	if err := domain.ValidateTenantID(tenantID); err != nil {
		return domain.Contact{}, err
	}
	if err := domain.ValidateUserID("user_id", userID); err != nil {
		return domain.Contact{}, err
	}
	var result domain.Contact
	now := s.now().UTC().Truncate(time.Microsecond)
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.contacts.GetForUpdate(ctx, tenantID, userID)
		if err != nil {
			return err
		}
		next, changed, err := cur.Apply(p, now)
		if err != nil {
			return err
		}
		if !changed {
			result = cur
			return errUnchanged
		}
		if err := s.contacts.Save(ctx, next); err != nil {
			return err
		}
		entry, err := domain.NewContactEntry(next, s.maxAttempts, now)
		if err != nil {
			return err
		}
		entry.TraceContext = observability.InjectTraceContext(ctx)
		if err := s.outbox.Coalesce(ctx, entry); err != nil {
			return err
		}
		result = next
		return nil
	})
	switch {
	case errors.Is(err, errUnchanged):
		return result, nil
	case err != nil:
		return domain.Contact{}, err
	}
	s.wake()
	return result, nil
}
