package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

func strp(s string) *string { return &s }

func TestContacts(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		if _, err := b.Contacts.Get(ctx, tenant, "usr_bob"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("Get before any contact: %v", err)
		}
		if _, err := b.Contacts.GetForUpdate(ctx, tenant, "usr_bob"); !errors.Is(err, repository.ErrTxRequired) {
			t.Errorf("GetForUpdate outside a transaction: %v", err)
		}
		at := t0.Add(123456 * time.Nanosecond)
		err := b.Tx.WithTx(ctx, func(ctx context.Context) error {
			c, err := b.Contacts.GetForUpdate(ctx, tenant, "usr_bob")
			if err != nil {
				return err
			}
			if !c.Empty() || !c.UpdatedAt.IsZero() || c.TenantID != tenant || c.UserID != "usr_bob" {
				t.Errorf("new contact = %+v", c)
			}
			c.Email, c.Phone, c.UpdatedAt = strp("bob@example.com"), strp("+14155550123"), at
			return b.Contacts.Save(ctx, c)
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Contacts.Get(ctx, tenant, "usr_bob")
		if err != nil || got.Email == nil || *got.Email != "bob@example.com" || got.SlackUserID != nil ||
			*got.Phone != "+14155550123" || !got.UpdatedAt.Equal(at.Truncate(time.Microsecond)) {
			t.Errorf("stored contact = %+v, %v", got, err)
		}
		// Another tenant, or another person, has none.
		if _, err := b.Contacts.Get(ctx, "tenant_other", "usr_bob"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("another tenant: %v", err)
		}
		if err := b.Contacts.Save(ctx, domain.Contact{TenantID: tenant, UserID: "usr_nobody", UpdatedAt: t0}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Save without a row: %v", err)
		}
		// A rolled-back GetForUpdate leaves nothing behind.
		_ = b.Tx.WithTx(ctx, func(ctx context.Context) error {
			if _, err := b.Contacts.GetForUpdate(ctx, tenant, "usr_carol"); err != nil {
				t.Error(err)
			}
			return errors.New("roll back")
		})
		if _, err := b.Contacts.Get(ctx, tenant, "usr_carol"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("rolled-back row: %v", err)
		}
	})
}

// TestContactUpdatesQueue: concurrent first-time updates of one person each
// see the other's result, so none is lost.
func TestContactUpdatesQueue(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		const writers = 8
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs <- b.Tx.WithTx(ctx, func(ctx context.Context) error {
					c, err := b.Contacts.GetForUpdate(ctx, tenant, "usr_bob")
					if err != nil {
						return err
					}
					// Each writer appends a digit to the phone number it read.
					p := "+1"
					if c.Phone != nil {
						p = *c.Phone
					}
					c.Phone, c.UpdatedAt = strp(p+string(rune('0'+i))), t0
					return b.Contacts.Save(ctx, c)
				})
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		got, err := b.Contacts.Get(ctx, tenant, "usr_bob")
		if err != nil || got.Phone == nil || len(*got.Phone) != 2+writers {
			t.Errorf("phone = %v, %v: an update was lost", got.Phone, err)
		}
	})
}

func contactEntry(t *testing.T, user, email string, at time.Time) domain.OutboxEntry {
	t.Helper()
	e, err := domain.NewContactEntry(domain.Contact{TenantID: tenant, UserID: user, Email: strp(email)}, 5, at)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func emailOf(t *testing.T, e domain.OutboxEntry) string {
	t.Helper()
	var u domain.ContactUpdate
	if err := json.Unmarshal(e.Payload, &u); err != nil || u.Email == nil {
		t.Fatalf("payload %s: %v", e.Payload, err)
	}
	return *u.Email
}

// TestOutboxCoalesce covers the one-row-per-key outbox used for contacts.
func TestOutboxCoalesce(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		first := contactEntry(t, "usr_bob", "a@example.com", t0)
		if err := b.Outbox.Coalesce(ctx, first); err != nil {
			t.Fatal(err)
		}
		// A second update before any send replaces the payload: one row.
		if err := b.Outbox.Coalesce(ctx, contactEntry(t, "usr_bob", "b@example.com", t0.Add(time.Second))); err != nil {
			t.Fatal(err)
		}
		rows, _ := b.Outbox.ListForAggregate(ctx, first.AggregateID)
		if len(rows) != 1 || emailOf(t, rows[0]) != "b@example.com" || rows[0].Status != domain.OutboxPending || rows[0].ID != first.ID {
			t.Fatalf("rows = %+v", rows)
		}

		// Claimed and sent unchanged: dispatched.
		claimed, _ := b.Outbox.ClaimBatch(ctx, t0.Add(2*time.Second), 10)
		if len(claimed) != 1 {
			t.Fatalf("claimed %d", len(claimed))
		}
		requeued, err := b.Outbox.MarkDispatchedIfUnchanged(ctx, claimed[0].ID, *claimed[0].ClaimedAt, claimed[0].Payload, t0.Add(3*time.Second))
		if err != nil || requeued {
			t.Fatalf("settle unchanged: %v, %v", requeued, err)
		}
		if got, _ := b.Outbox.Get(ctx, first.ID); got.Status != domain.OutboxDispatched || got.DispatchedAt == nil {
			t.Errorf("after send = %+v", got)
		}

		// A dispatched row takes a new update and is pending again.
		if err := b.Outbox.Coalesce(ctx, contactEntry(t, "usr_bob", "c@example.com", t0.Add(4*time.Second))); err != nil {
			t.Fatal(err)
		}
		got, _ := b.Outbox.Get(ctx, first.ID)
		if got.Status != domain.OutboxPending || got.DispatchedAt != nil || got.Attempts != 0 || emailOf(t, got) != "c@example.com" ||
			!got.NextAttemptAt.Equal(t0.Add(4*time.Second)) {
			t.Errorf("after update of a sent row = %+v", got)
		}

		// An update while the row is being sent keeps the claim; settling
		// then returns the row to pending with the newer payload.
		claimed, _ = b.Outbox.ClaimBatch(ctx, t0.Add(5*time.Second), 10)
		if len(claimed) != 1 {
			t.Fatalf("claimed %d", len(claimed))
		}
		if err := b.Outbox.Coalesce(ctx, contactEntry(t, "usr_bob", "d@example.com", t0.Add(6*time.Second))); err != nil {
			t.Fatal(err)
		}
		held, _ := b.Outbox.Get(ctx, first.ID)
		if held.Status != domain.OutboxProcessing || held.ClaimedAt == nil || !held.ClaimedAt.Equal(*claimed[0].ClaimedAt) {
			t.Fatalf("update during a send took the claim: %+v", held)
		}
		if again, _ := b.Outbox.ClaimBatch(ctx, t0.Add(7*time.Second), 10); len(again) != 0 {
			t.Fatal("a row being sent was claimed twice")
		}
		requeued, err = b.Outbox.MarkDispatchedIfUnchanged(ctx, claimed[0].ID, *claimed[0].ClaimedAt, claimed[0].Payload, t0.Add(8*time.Second))
		if err != nil || !requeued {
			t.Fatalf("settle changed: %v, %v", requeued, err)
		}
		got, _ = b.Outbox.Get(ctx, first.ID)
		if got.Status != domain.OutboxPending || emailOf(t, got) != "d@example.com" || got.ClaimedAt != nil || !got.NextAttemptAt.Equal(t0.Add(8*time.Second)) {
			t.Errorf("after settling a changed row = %+v", got)
		}

		// A rejection with a newer payload folded in is not dead.
		claimed, _ = b.Outbox.ClaimBatch(ctx, t0.Add(9*time.Second), 10)
		if err := b.Outbox.Coalesce(ctx, contactEntry(t, "usr_bob", "e@example.com", t0.Add(10*time.Second))); err != nil {
			t.Fatal(err)
		}
		requeued, err = b.Outbox.MarkDeadLetterIfUnchanged(ctx, claimed[0].ID, *claimed[0].ClaimedAt, claimed[0].Payload, 1, "400", t0.Add(11*time.Second))
		if err != nil || !requeued {
			t.Fatalf("reject changed: %v, %v", requeued, err)
		}
		if got, _ := b.Outbox.Get(ctx, first.ID); got.Status != domain.OutboxPending || got.Attempts != 0 || got.LastError != "" {
			t.Errorf("after rejecting a changed row = %+v", got)
		}
		// Without one it is.
		claimed, _ = b.Outbox.ClaimBatch(ctx, t0.Add(12*time.Second), 10)
		requeued, err = b.Outbox.MarkDeadLetterIfUnchanged(ctx, claimed[0].ID, *claimed[0].ClaimedAt, claimed[0].Payload, 1, "400 bad email", t0.Add(13*time.Second))
		if err != nil || requeued {
			t.Fatalf("reject unchanged: %v, %v", requeued, err)
		}
		got, _ = b.Outbox.Get(ctx, first.ID)
		if got.Status != domain.OutboxDeadLetter || got.LastError != "400 bad email" || got.Attempts != 1 {
			t.Errorf("after rejecting = %+v", got)
		}
		// A dead row comes back with the next update.
		if err := b.Outbox.Coalesce(ctx, contactEntry(t, "usr_bob", "f@example.com", t0.Add(14*time.Second))); err != nil {
			t.Fatal(err)
		}
		if got, _ := b.Outbox.Get(ctx, first.ID); got.Status != domain.OutboxPending || got.Attempts != 0 || got.LastError != "" {
			t.Errorf("dead row after an update = %+v", got)
		}

		// A stale claim settles nothing.
		requeued, err = b.Outbox.MarkDispatchedIfUnchanged(ctx, first.ID, t0, claimed[0].Payload, t0.Add(15*time.Second))
		if err != nil || requeued {
			t.Errorf("stale claim: %v, %v", requeued, err)
		}
		if got, _ := b.Outbox.Get(ctx, first.ID); got.Status != domain.OutboxPending {
			t.Errorf("a stale claim changed the row: %+v", got)
		}
		// Each person has their own row.
		if err := b.Outbox.Coalesce(ctx, contactEntry(t, "usr_carol", "c@example.com", t0)); err != nil {
			t.Fatal(err)
		}
		if n, _ := b.Outbox.CountByStatus(ctx, domain.OutboxPending); n != 2 {
			t.Errorf("pending rows = %d, want 2", n)
		}
	})
}

// TestOutboxCoalesceRacesSettle: updates and sends of one row, interleaved
// by concurrent goroutines, always end with the latest payload sent last or
// still pending, never dispatched behind a newer payload.
func TestOutboxCoalesceRacesSettle(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		const updates = 40
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			sent    []string
			done    = make(chan struct{})
			lastErr error
		)
		wg.Add(1)
		go func() { // the relay: claim, "send", settle, until updates stop
			defer wg.Done()
			for {
				claimed, err := b.Outbox.ClaimBatch(ctx, time.Now().Add(time.Hour), 10)
				if err != nil {
					mu.Lock()
					lastErr = err
					mu.Unlock()
					return
				}
				for _, e := range claimed {
					mu.Lock()
					sent = append(sent, emailOf(t, e))
					mu.Unlock()
					if _, err := b.Outbox.MarkDispatchedIfUnchanged(ctx, e.ID, *e.ClaimedAt, e.Payload, time.Now()); err != nil {
						mu.Lock()
						lastErr = err
						mu.Unlock()
						return
					}
				}
				if len(claimed) == 0 {
					select {
					case <-done:
						return
					default:
						time.Sleep(time.Millisecond)
					}
				}
			}
		}()
		var last string
		for i := 0; i < updates; i++ {
			last = "u" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.com"
			if err := b.Outbox.Coalesce(ctx, contactEntry(t, "usr_bob", last, time.Now())); err != nil {
				t.Fatal(err)
			}
		}
		close(done)
		wg.Wait()
		if lastErr != nil {
			t.Fatal(lastErr)
		}
		// Whatever was not sent yet is pending with the latest payload.
		rows, _ := b.Outbox.ListForAggregate(ctx, domain.ContactAggregateID(tenant, "usr_bob"))
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		row := rows[0]
		switch row.Status {
		case domain.OutboxDispatched:
			if len(sent) == 0 || sent[len(sent)-1] != last {
				t.Errorf("dispatched, but the last send was %v, want %s", sent, last)
			}
		case domain.OutboxPending:
			if emailOf(t, row) != last {
				t.Errorf("pending with %s, want %s", emailOf(t, row), last)
			}
		default:
			t.Errorf("row status %s", row.Status)
		}
	})
}

// TestWebhookCallbacks: callbackIds are remembered per tenant (AP-06), and
// concurrent records of one ID report it new exactly once.
func TestWebhookCallbacks(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		const id = "cb-0123456789abcdef0123456789abcdef"
		if seen, err := b.Deliveries.CallbackSeen(ctx, tenant, id); err != nil || seen {
			t.Fatalf("seen before any record: %v, %v", seen, err)
		}
		if fresh, err := b.Deliveries.RecordCallback(ctx, tenant, id, t0); err != nil || !fresh {
			t.Fatalf("first record = %v, %v", fresh, err)
		}
		if fresh, _ := b.Deliveries.RecordCallback(ctx, tenant, id, t0.Add(time.Hour)); fresh {
			t.Error("a repeated callbackId was reported as new")
		}
		if seen, _ := b.Deliveries.CallbackSeen(ctx, tenant, id); !seen {
			t.Error("a recorded callbackId is not seen")
		}
		// Another tenant's same ID is its own.
		if fresh, _ := b.Deliveries.RecordCallback(ctx, "tenant_other", id, t0); !fresh {
			t.Error("callbackIds are not scoped to the tenant")
		}
		// Signatures and callbackIds are separate records.
		if fresh, _ := b.Deliveries.Record(ctx, id, t0); !fresh {
			t.Error("a signature collided with a callbackId")
		}
		if n, err := b.Deliveries.DeleteCallbacksBefore(ctx, t0.Add(time.Minute)); err != nil || n != 2 {
			t.Errorf("DeleteCallbacksBefore = %d, %v", n, err)
		}
		if seen, _ := b.Deliveries.CallbackSeen(ctx, tenant, id); seen {
			t.Error("a deleted callbackId is still seen")
		}

		const racers = 8
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			fresh int
		)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := b.Tx.WithTx(ctx, func(ctx context.Context) error {
					ok, err := b.Deliveries.RecordCallback(ctx, tenant, "cb-ffffffffffffffffffffffffffffffff", t0)
					if ok {
						mu.Lock()
						fresh++
						mu.Unlock()
					}
					return err
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if fresh != 1 {
			t.Errorf("%d concurrent records reported new, want 1", fresh)
		}
	})
}
