package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

// TestMyContact is AP-05's API: a person reads and sets only their own
// contact, and each change waits in the outbox for AgentPulse.
func TestMyContact(t *testing.T) {
	e := newEnv(t)

	r := e.do(req{method: "GET", path: "/api/v1/me/contact"})
	if r.status != http.StatusOK || string(r.raw) != `{"email":null,"slack_user_id":null,"phone":null,"updated_at":null}`+"\n" {
		t.Fatalf("no contact yet: %d %s", r.status, r.raw)
	}
	r = e.do(req{method: "PUT", path: "/api/v1/me/contact", body: map[string]any{
		"email": "bob@example.com", "slack_user_id": "U024BE7LH", "phone": "+14155550123"}})
	if r.status != http.StatusOK || r.json["email"] != "bob@example.com" || r.json["slack_user_id"] != "U024BE7LH" ||
		r.json["phone"] != "+14155550123" || r.json["updated_at"] != e.now.Format(time.RFC3339) {
		t.Fatalf("put: %d %s", r.status, r.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/me/contact"}); got.json["email"] != "bob@example.com" {
		t.Errorf("get after put: %s", got.raw)
	}

	// An absent field is kept; null clears.
	r = e.do(req{method: "PUT", path: "/api/v1/me/contact", body: map[string]any{"phone": nil}})
	if r.status != http.StatusOK || r.json["email"] != "bob@example.com" || r.json["phone"] != nil {
		t.Errorf("partial put: %d %s", r.status, r.raw)
	}

	// The change waits in the outbox as the person's one contact row, with
	// the whole contact.
	rows, err := e.b.Outbox.ListForAggregate(context.Background(), domain.ContactAggregateID(tenant, "usr_bob"))
	if err != nil || len(rows) != 1 || rows[0].EventType != domain.EventContactUpdate {
		t.Fatalf("outbox = %+v, %v", rows, err)
	}
	var u domain.ContactUpdate
	if err := json.Unmarshal(rows[0].Payload, &u); err != nil || u.UserID != "usr_bob" || *u.Email != "bob@example.com" ||
		*u.SlackUserID != "U024BE7LH" || u.Phone != nil {
		t.Errorf("payload = %s", rows[0].Payload)
	}

	// Nobody else sees or changes it: the caller is the only subject.
	other := e.do(req{method: "GET", path: "/api/v1/me/contact", as: "admin"})
	if other.status != http.StatusOK || other.json["email"] != nil {
		t.Errorf("another user's view: %s", other.raw)
	}
	if r := e.do(req{method: "PUT", path: "/api/v1/me/contact", as: "admin", body: map[string]any{"user_id": "usr_bob", "email": "evil@example.com"}}); r.status != http.StatusBadRequest {
		t.Errorf("naming another user: %d %s", r.status, r.raw)
	}
	if r := e.do(req{method: "PUT", path: "/api/v1/me/contact", as: "admin", body: map[string]any{"email": "admin@example.com"}}); r.status != http.StatusOK {
		t.Fatalf("admin's own contact: %d %s", r.status, r.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/me/contact"}); got.json["email"] != "bob@example.com" {
		t.Errorf("bob's contact after the admin's put: %s", got.raw)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/me/contact", as: "other-tenant"}); got.json["email"] != nil {
		t.Errorf("another tenant's view: %s", got.raw)
	}

	// Validation.
	for name, body := range map[string]any{
		"bad email":    map[string]any{"email": "Bob <bob@example.com>"},
		"bad slack id": map[string]any{"slack_user_id": "bob"},
		"bad phone":    map[string]any{"phone": "555-0123"},
	} {
		if r := e.do(req{method: "PUT", path: "/api/v1/me/contact", body: body}); r.status != http.StatusUnprocessableEntity || r.errCode() != CodeValidation {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	for name, body := range map[string]any{
		"not JSON":    "{nope",
		"number":      map[string]any{"email": 3},
		"camelCase":   map[string]any{"slackUserId": "U024BE7LH"},
		"two objects": `{"email":null}{"email":null}`,
	} {
		if r := e.do(req{method: "PUT", path: "/api/v1/me/contact", body: body}); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	if r := e.do(req{method: "PUT", path: "/api/v1/me/contact", as: "none", body: map[string]any{}}); r.status != http.StatusUnauthorized {
		t.Errorf("no credentials: %d", r.status)
	}
	if got := e.do(req{method: "GET", path: "/api/v1/me/contact"}); got.json["email"] != "bob@example.com" || got.json["slack_user_id"] != "U024BE7LH" {
		t.Errorf("a refused put changed the contact: %s", got.raw)
	}
}
