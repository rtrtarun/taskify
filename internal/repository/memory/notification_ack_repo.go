package memory

import (
	"context"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

type ackKey struct{ tenant, user, notification string }

// NotificationAckRepo implements repository.NotificationAckRepository.
type NotificationAckRepo struct{ st *Store }

// NotificationAcks returns the notification ack repository.
func (st *Store) NotificationAcks() *NotificationAckRepo { return &NotificationAckRepo{st} }

var _ repository.NotificationAckRepository = (*NotificationAckRepo)(nil)

func (r *NotificationAckRepo) Save(ctx context.Context, a domain.NotificationAck) (changed bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		k := ackKey{a.TenantID, a.UserID, a.NotificationID}
		if cur, ok := s.acks[k]; ok && cur.Signal == a.Signal && cur.SnoozeDuration == a.SnoozeDuration {
			return nil
		}
		a.AckedAt = micro(a.AckedAt)
		if a.SnoozeUntil != nil {
			u := micro(*a.SnoozeUntil)
			a.SnoozeUntil = &u
		}
		s.acks[k] = a
		changed = true
		return nil
	})
	return changed, err
}

func (r *NotificationAckRepo) ListFor(ctx context.Context, tenantID, userID string, ids []string) (map[string]domain.NotificationAck, error) {
	out := map[string]domain.NotificationAck{}
	err := r.st.do(ctx, func(s *state) error {
		for _, id := range ids {
			if a, ok := s.acks[ackKey{tenantID, userID, id}]; ok {
				out[id] = a
			}
		}
		return nil
	})
	return out, err
}
