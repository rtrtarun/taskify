package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

func (r faultDeliveries) DeleteCallbacksBefore(ctx context.Context, cutoff time.Time) (int, error) {
	if err := r.f.hit("Deliveries.DeleteCallbacksBefore"); err != nil {
		return 0, err
	}
	return r.WebhookDeliveryRepository.DeleteCallbacksBefore(ctx, cutoff)
}

// TestMalformedContactRowIsDeadLettered: a contact.update row that names no
// person cannot be sent, and retrying cannot help, so it goes to dead letter
// at once without a call to AgentPulse.
func TestMalformedContactRowIsDeadLettered(t *testing.T) {
	for name, payload := range map[string]string{
		"not JSON": `"contact"`,
		"no user":  `{"userId": "", "email": "bob@example.com"}`,
	} {
		t.Run(name, func(t *testing.T) {
			each(t, func(t *testing.T, b repotest.Backend) {
				mock, client := newMock(t)
				entry, err := domain.NewContactEntry(domain.Contact{TenantID: tenant, UserID: "usr_bob"}, 10, t0)
				if err != nil {
					t.Fatal(err)
				}
				entry.Payload = json.RawMessage(payload)
				if err := b.Outbox.Coalesce(ctx, entry); err != nil {
					t.Fatal(err)
				}
				r := NewRelay(b.Outbox, client, RelayConfig{Now: func() time.Time { return t0 }, Logger: quiet})
				if n, err := r.RunOnce(ctx); err != nil || n != 1 {
					t.Fatalf("run: %d rows, %v", n, err)
				}
				row := contactRow(t, b)
				if row.Status != domain.OutboxDeadLetter || row.Attempts != 1 || row.LastError != errMalformedRow.Error() {
					t.Errorf("row = %s after %d attempts: %q", row.Status, row.Attempts, row.LastError)
				}
				if n := len(mock.Requests()); n != 0 {
					t.Errorf("AgentPulse received %d requests", n)
				}
			})
		})
	}
}

// TestCleanupCallbackFailure: when expired callbackIds cannot be deleted,
// the run stops after the delivery records, reporting them, and the next
// run deletes the callbackIds and the rows.
func TestCleanupCallbackFailure(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		now := t0.Add(30 * 24 * time.Hour)
		seedOutbox(t, b, 1, 10, t0)
		claimed, _ := b.Outbox.ClaimBatch(ctx, t0, 10)
		if err := b.Outbox.MarkDispatched(ctx, claimed[0].ID, *claimed[0].ClaimedAt, now.Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Deliveries.Record(ctx, "old-sig", now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		const cb = "cb-00000000000000000000000000000001"
		if _, err := b.Deliveries.RecordCallback(ctx, tenant, cb, now.Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		fs := newFaults()
		fs.fail("Deliveries.DeleteCallbacksBefore", 0, 1)
		c := NewCleanup(b.Outbox, CleanupConfig{Deliveries: faultDeliveries{b.Deliveries, fs}, Now: func() time.Time { return now }, Logger: quiet})

		if n, err := c.RunOnce(ctx); !errors.Is(err, errInjected) || n != 1 {
			t.Fatalf("RunOnce = %d, %v; want the delivery record and the injected failure", n, err)
		}
		if seen, _ := b.Deliveries.CallbackSeen(ctx, tenant, cb); !seen {
			t.Error("the callbackId was deleted although the delete failed")
		}
		if got := count(t, b, domain.OutboxDispatched); got != 1 {
			t.Errorf("dispatched rows = %d, want the row kept for the next run", got)
		}
		if n, err := c.RunOnce(ctx); err != nil || n != 2 {
			t.Errorf("RunOnce = %d, %v; want the callbackId and the row", n, err)
		}
		if seen, _ := b.Deliveries.CallbackSeen(ctx, tenant, cb); seen {
			t.Error("the expired callbackId was kept")
		}
	})
}
