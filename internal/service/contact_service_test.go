package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

func setField(v string) domain.ContactField { return domain.ContactField{Set: true, Value: &v} }

// contactRows returns user's contact.update outbox rows.
func contactRows(t *testing.T, b repotest.Backend, user string) []domain.OutboxEntry {
	t.Helper()
	rows, err := b.Outbox.ListForAggregate(ctx, domain.ContactAggregateID(tenant, user))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// forwarded is the contact a contact.update row sends.
func forwarded(t *testing.T, row domain.OutboxEntry) domain.ContactUpdate {
	t.Helper()
	var u domain.ContactUpdate
	if err := json.Unmarshal(row.Payload, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestContactUpdateForwardsTheWholeContact is AP-05 in the service: a
// change stores the contact and writes the person's one contact.update row,
// carrying the whole latest contact, and a later change rewrites that row;
// a change to nothing new writes nothing and wakes nobody.
func TestContactUpdateForwardsTheWholeContact(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		wakes := 0
		cs := NewContactService(f.b.Tx, f.b.Contacts, f.b.Outbox, 4, func() time.Time { return f.now }, func() { wakes++ })

		got, err := cs.Get(ctx, tenant, bob.ID)
		if err != nil || !got.Empty() || got.TenantID != tenant || got.UserID != bob.ID || !got.UpdatedAt.IsZero() {
			t.Fatalf("a contact never set = %+v, %v; want an empty one", got, err)
		}

		c, err := cs.Update(ctx, tenant, bob.ID, domain.ContactPatch{
			Email: setField(" bob@example.com "), SlackUserID: setField("U024BE7LH"), Phone: setField("+14155550123")})
		if err != nil {
			t.Fatal(err)
		}
		if strOf(c.Email) != "bob@example.com" || strOf(c.SlackUserID) != "U024BE7LH" || strOf(c.Phone) != "+14155550123" ||
			!c.UpdatedAt.Equal(f.now) {
			t.Errorf("updated contact = %+v", c)
		}
		if stored, err := cs.Get(ctx, tenant, bob.ID); err != nil || strOf(stored.Email) != "bob@example.com" || !stored.UpdatedAt.Equal(f.now) {
			t.Errorf("stored contact = %+v, %v", stored, err)
		}
		rows := contactRows(t, f.b, bob.ID)
		if len(rows) != 1 {
			t.Fatalf("contact rows = %d, want 1", len(rows))
		}
		first := rows[0]
		if first.EventType != domain.EventContactUpdate || first.IdempotencyKey != domain.ContactIdempotencyKey(tenant, bob.ID) ||
			first.TenantID != tenant || first.MaxAttempts != 4 || first.Status != domain.OutboxPending {
			t.Errorf("row = %+v", first)
		}
		if u := forwarded(t, first); u.UserID != bob.ID || strOf(u.Email) != "bob@example.com" || strOf(u.SlackUserID) != "U024BE7LH" ||
			strOf(u.Phone) != "+14155550123" {
			t.Errorf("forwarded = %+v", u)
		}
		if wakes != 1 {
			t.Errorf("wakes = %d, want 1", wakes)
		}

		// Clearing the phone forwards the whole contact again, in the same row.
		f.now = f.now.Add(time.Minute)
		c, err = cs.Update(ctx, tenant, bob.ID, domain.ContactPatch{Phone: domain.ContactField{Set: true}})
		if err != nil || c.Phone != nil || strOf(c.Email) != "bob@example.com" || !c.UpdatedAt.Equal(f.now) {
			t.Fatalf("cleared phone = %+v, %v", c, err)
		}
		rows = contactRows(t, f.b, bob.ID)
		if len(rows) != 1 || rows[0].ID != first.ID {
			t.Fatalf("contact rows = %+v, want the first row reused", rows)
		}
		if u := forwarded(t, rows[0]); u.Phone != nil || strOf(u.Email) != "bob@example.com" || strOf(u.SlackUserID) != "U024BE7LH" {
			t.Errorf("forwarded = %+v", u)
		}
		if wakes != 2 {
			t.Errorf("wakes = %d, want 2", wakes)
		}

		// The same email again, and clearing a field that is not set: nothing.
		f.now = f.now.Add(time.Minute)
		same, err := cs.Update(ctx, tenant, bob.ID, domain.ContactPatch{Email: setField("bob@example.com"), Phone: domain.ContactField{Set: true}})
		if err != nil || !same.UpdatedAt.Equal(c.UpdatedAt) || strOf(same.Email) != "bob@example.com" {
			t.Errorf("unchanged = %+v, %v; want the stored contact", same, err)
		}
		if u := forwarded(t, contactRows(t, f.b, bob.ID)[0]); u.Phone != nil || strOf(u.Email) != "bob@example.com" {
			t.Errorf("an unchanged contact rewrote the row: %+v", u)
		}
		if wakes != 2 {
			t.Errorf("an unchanged contact woke the relay")
		}

		// People and tenants are kept apart.
		if carol, err := cs.Get(ctx, tenant, "usr_carol"); err != nil || !carol.Empty() {
			t.Errorf("carol = %+v, %v", carol, err)
		}
		if other, err := cs.Get(ctx, "tenant_other", bob.ID); err != nil || !other.Empty() {
			t.Errorf("bob in another tenant = %+v, %v", other, err)
		}
	})
}

// TestContactUpdateRefusals: a refused or empty change leaves no contact
// row behind, not even the empty one the lock creates, and sends nothing.
func TestContactUpdateRefusals(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		woken := false
		cs := NewContactService(f.b.Tx, f.b.Contacts, f.b.Outbox, 0, nil, func() { woken = true })
		email := domain.ContactPatch{Email: setField("bob@example.com")}
		for name, tc := range map[string]struct {
			tenant, user string
			p            domain.ContactPatch
		}{
			"a reserved tenant":  {"default", bob.ID, email},
			"a malformed tenant": {"tenant acme", bob.ID, email},
			"no user":            {tenant, "", email},
			"a non-ASCII user":   {tenant, "usr_bobé", email},
			"a display name":     {tenant, bob.ID, domain.ContactPatch{Email: setField("Bob <bob@example.com>")}},
			"a bad Slack ID":     {tenant, bob.ID, domain.ContactPatch{Email: setField("bob@example.com"), SlackUserID: setField("bob")}},
			"a bad phone":        {tenant, bob.ID, domain.ContactPatch{Phone: setField("0123")}},
		} {
			if _, err := cs.Update(ctx, tc.tenant, tc.user, tc.p); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("%s: error = %v, want a validation error", name, err)
			}
		}
		if got, err := cs.Update(ctx, tenant, bob.ID, domain.ContactPatch{Email: domain.ContactField{Set: true}}); err != nil || !got.Empty() {
			t.Errorf("clearing an unset field = %+v, %v", got, err)
		}
		if _, err := f.b.Contacts.Get(ctx, tenant, bob.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("a refused change left a contact row: %v", err)
		}
		if rows := contactRows(t, f.b, bob.ID); len(rows) != 0 || woken {
			t.Errorf("a refused change wrote %d rows, woken %v", len(rows), woken)
		}

		// With the defaults, a change is forwarded with 10 attempts.
		if _, err := cs.Update(ctx, tenant, bob.ID, email); err != nil {
			t.Fatal(err)
		}
		if rows := contactRows(t, f.b, bob.ID); len(rows) != 1 || rows[0].MaxAttempts != 10 || !woken {
			t.Errorf("rows = %+v, woken %v", rows, woken)
		}
	})
}

type faultContacts struct {
	repository.ContactRepository
	in *injector
}

func (f faultContacts) GetForUpdate(ctx context.Context, tenantID, userID string) (domain.Contact, error) {
	if err := f.in.hit(); err != nil {
		return domain.Contact{}, err
	}
	return f.ContactRepository.GetForUpdate(ctx, tenantID, userID)
}
func (f faultContacts) Save(ctx context.Context, c domain.Contact) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.ContactRepository.Save(ctx, c)
}

type faultCoalesce struct {
	repository.OutboxRepository
	in *injector
}

func (f faultCoalesce) Coalesce(ctx context.Context, e domain.OutboxEntry) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.OutboxRepository.Coalesce(ctx, e)
}

// TestContactUpdateFailuresLeaveNoTrace: the contact and its outbox row
// commit together, so a failure at any step keeps the previous contact and
// the row that forwards it.
func TestContactUpdateFailuresLeaveNoTrace(t *testing.T) {
	for failAt := 1; ; failAt++ {
		b := repotest.Memory()
		if _, err := NewContactService(b.Tx, b.Contacts, b.Outbox, 10, nil, nil).Update(ctx, tenant, bob.ID,
			domain.ContactPatch{Email: setField("old@example.com")}); err != nil {
			t.Fatal(err)
		}
		in := &injector{failAt: failAt}
		woken := false
		cs := NewContactService(b.Tx, faultContacts{b.Contacts, in}, faultCoalesce{b.Outbox, in}, 10, nil, func() { woken = true })
		_, err := cs.Update(ctx, tenant, bob.ID, domain.ContactPatch{Email: setField("new@example.com")})
		if in.calls < failAt {
			if err != nil || failAt == 1 {
				t.Fatalf("clean run: %v after %d calls", err, in.calls)
			}
			return
		}
		if !errors.Is(err, errInjected) {
			t.Errorf("failing call %d: error = %v, want the injected failure", failAt, err)
		}
		if c, _ := b.Contacts.Get(ctx, tenant, bob.ID); strOf(c.Email) != "old@example.com" {
			t.Errorf("failing call %d: stored email = %s", failAt, strOf(c.Email))
		}
		if rows := contactRows(t, b, bob.ID); len(rows) != 1 || strOf(forwarded(t, rows[0]).Email) != "old@example.com" {
			t.Errorf("failing call %d: rows = %+v", failAt, rows)
		}
		if woken {
			t.Errorf("failing call %d woke the relay", failAt)
		}
	}
}
