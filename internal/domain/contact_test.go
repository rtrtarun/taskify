package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func set(v string) ContactField { return ContactField{Set: true, Value: &v} }

var clearField = ContactField{Set: true}

func TestContactApply(t *testing.T) {
	empty := Contact{TenantID: "tenant_acme", UserID: "usr_bob"}
	c, changed, err := empty.Apply(ContactPatch{Email: set(" bob@example.com "), SlackUserID: set("U024BE7LH"), Phone: set("+14155550123")}, t0)
	if err != nil || !changed {
		t.Fatalf("Apply = %v, %v", changed, err)
	}
	if *c.Email != "bob@example.com" || *c.SlackUserID != "U024BE7LH" || *c.Phone != "+14155550123" || !c.UpdatedAt.Equal(t0) {
		t.Errorf("contact = %+v", c)
	}
	if !empty.Empty() || c.Empty() {
		t.Error("Empty is wrong")
	}

	// An absent field is left alone; null and "" clear.
	next, changed, err := c.Apply(ContactPatch{Phone: clearField, SlackUserID: set("  ")}, t0.Add(1))
	if err != nil || !changed || *next.Email != "bob@example.com" || next.Phone != nil || next.SlackUserID != nil {
		t.Errorf("partial update = %+v, %v, %v", next, changed, err)
	}
	// The same values again change nothing.
	if same, changed, err := c.Apply(ContactPatch{Email: set("bob@example.com"), Phone: set("+14155550123")}, t0.Add(2)); err != nil || changed || !same.UpdatedAt.Equal(t0) {
		t.Errorf("repeat = %+v, %v, %v", same, changed, err)
	}
	if _, changed, _ := c.Apply(ContactPatch{}, t0); changed {
		t.Error("an empty patch changed the contact")
	}

	for name, p := range map[string]ContactPatch{
		"display name":     {Email: set("Bob <bob@example.com>")},
		"no domain":        {Email: set("bob")},
		"long email":       {Email: set(strings.Repeat("b", 250) + "@x.io")},
		"lower-case slack": {SlackUserID: set("u024be7lh")},
		"slack channel":    {SlackUserID: set("C024BE7LH")},
		"local phone":      {Phone: set("4155550123")},
		"phone with dash":  {Phone: set("+1-415-555-0123")},
	} {
		if _, _, err := c.Apply(p, t0); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	// One bad field fails the whole patch.
	if got, changed, err := c.Apply(ContactPatch{Email: set("new@example.com"), Phone: set("x")}, t0); err == nil || changed || *got.Email != "bob@example.com" {
		t.Errorf("mixed patch = %+v, %v, %v", got, changed, err)
	}
}

func TestNewContactEntry(t *testing.T) {
	email := "bob@example.com"
	e, err := NewContactEntry(Contact{TenantID: "tenant_acme", UserID: "usr_bob", Email: &email}, 7, t0)
	if err != nil {
		t.Fatal(err)
	}
	if e.EventType != EventContactUpdate || e.IdempotencyKey != "taskify:tenant_acme:contact.update:usr_bob" ||
		e.AggregateID != ContactAggregateID("tenant_acme", "usr_bob") || e.MaxAttempts != 7 || e.Status != OutboxPending {
		t.Errorf("entry = %+v", e)
	}
	// Every field is present, null when unset.
	if string(e.Payload) != `{"userId":"usr_bob","email":"bob@example.com","slackUserId":null,"phone":null}` {
		t.Errorf("payload = %s", e.Payload)
	}
	var u ContactUpdate
	if err := json.Unmarshal(e.Payload, &u); err != nil || u.UserID != "usr_bob" {
		t.Errorf("payload does not round-trip: %v", err)
	}
	if ContactAggregateID("tenant_acme", "usr_bob") == ContactAggregateID("tenant_acme", "usr_carol") ||
		ContactAggregateID("a", "bc") == ContactAggregateID("ab", "c") {
		t.Error("aggregate IDs collide")
	}
	if _, err := NewContactEntry(Contact{TenantID: "default", UserID: "u"}, 1, t0); !errors.Is(err, ErrValidation) {
		t.Errorf("reserved tenant: %v", err)
	}
	if _, err := NewContactEntry(Contact{TenantID: "tenant_acme"}, 1, t0); !errors.Is(err, ErrValidation) {
		t.Errorf("no user: %v", err)
	}
}
