package repotest

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

func TestNotificationAcks(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		ack := func(tenantID, user, id, signal string, mins int) domain.NotificationAck {
			a := domain.NotificationAck{TenantID: tenantID, UserID: user, NotificationID: id, Signal: signal, AckedAt: t0.Add(time.Duration(mins) * time.Minute)}
			if signal == domain.AckSnoozed {
				a.SnoozeDuration = "2h"
				a.SnoozeUntil = ptr(a.AckedAt.Add(2 * time.Hour))
			}
			return a
		}
		save := func(a domain.NotificationAck) bool {
			t.Helper()
			changed, err := b.Acks.Save(ctx, a)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			return changed
		}

		if !save(ack(tenant, "usr_a", "ntf-1", domain.AckActed, 1)) {
			t.Error("a first reaction is a change")
		}
		if save(ack(tenant, "usr_a", "ntf-1", domain.AckActed, 9)) {
			t.Error("the same reaction again is not a change")
		}
		got, err := b.Acks.ListFor(ctx, tenant, "usr_a", []string{"ntf-1", "ntf-none"})
		if err != nil || len(got) != 1 || !got["ntf-1"].AckedAt.Equal(t0.Add(time.Minute)) || got["ntf-1"].Signal != domain.AckActed {
			t.Fatalf("ListFor = %+v, %v (a repeat must keep the first time)", got, err)
		}

		if !save(ack(tenant, "usr_a", "ntf-1", domain.AckSnoozed, 3)) {
			t.Error("a different reaction is a change")
		}
		got, _ = b.Acks.ListFor(ctx, tenant, "usr_a", []string{"ntf-1"})
		s := got["ntf-1"]
		if s.Signal != domain.AckSnoozed || s.SnoozeDuration != "2h" || s.SnoozeUntil == nil || !s.SnoozeUntil.Equal(t0.Add(3*time.Minute+2*time.Hour)) {
			t.Errorf("snoozed ack = %+v", s)
		}
		// A different snooze duration is a different reaction.
		longer := ack(tenant, "usr_a", "ntf-1", domain.AckSnoozed, 4)
		longer.SnoozeDuration = "4h"
		if !save(longer) {
			t.Error("a new snooze duration is a change")
		}

		// Reactions are per person, per tenant, per notification.
		save(ack(tenant, "usr_b", "ntf-1", domain.AckDismissed, 5))
		save(ack("tenant_other", "usr_a", "ntf-1", domain.AckDismissed, 5))
		if got, _ := b.Acks.ListFor(ctx, tenant, "usr_a", []string{"ntf-1"}); got["ntf-1"].Signal != domain.AckSnoozed {
			t.Errorf("another person's or tenant's ack leaked: %+v", got)
		}
		if got, _ := b.Acks.ListFor(ctx, tenant, "usr_c", []string{"ntf-1"}); len(got) != 0 {
			t.Errorf("a person with no ack sees %+v", got)
		}
		if got, err := b.Acks.ListFor(ctx, tenant, "usr_a", nil); err != nil || got == nil || len(got) != 0 {
			t.Errorf("ListFor(nil) = %v, %v", got, err)
		}
	})
}

// TestNotificationAckRace: of many concurrent saves of one reaction, exactly
// one reports a change, so exactly one feedback row is written.
func TestNotificationAckRace(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		a := domain.NotificationAck{TenantID: tenant, UserID: "usr_a", NotificationID: "ntf-race", Signal: domain.AckDismissed, AckedAt: t0}
		var changed atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := b.Acks.Save(ctx, a)
				if err != nil {
					t.Errorf("Save: %v", err)
				}
				if c {
					changed.Add(1)
				}
			}()
		}
		wg.Wait()
		if changed.Load() != 1 {
			t.Errorf("%d saves reported a change, want 1", changed.Load())
		}
	})
}
