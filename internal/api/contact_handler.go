package api

import (
	"net/http"

	"github.com/rtrtarun/taskify/internal/domain"
)

// contactJSON is a person's contact. A field is null when unset;
// updated_at is null for a person who never set a contact.
type contactJSON struct {
	Email       *string `json:"email"`
	SlackUserID *string `json:"slack_user_id"`
	Phone       *string `json:"phone"`
	UpdatedAt   *string `json:"updated_at"`
}

func toContactJSON(c domain.Contact) contactJSON {
	out := contactJSON{Email: c.Email, SlackUserID: c.SlackUserID, Phone: c.Phone}
	if !c.UpdatedAt.IsZero() {
		at := rfc3339(c.UpdatedAt)
		out.UpdatedAt = &at
	}
	return out
}

// contactRequest is PUT /me/contact. Each field is optional: an absent field
// is left as it is, and null (or an empty string) clears it.
type contactRequest struct {
	Email       optional[string] `json:"email"`
	SlackUserID optional[string] `json:"slack_user_id"`
	Phone       optional[string] `json:"phone"`
}

func (c contactRequest) patch() domain.ContactPatch {
	field := func(o optional[string]) domain.ContactField {
		if !o.Set {
			return domain.ContactField{}
		}
		if o.Null {
			return domain.ContactField{Set: true}
		}
		v := o.Value
		return domain.ContactField{Set: true, Value: &v}
	}
	return domain.ContactPatch{Email: field(c.Email), SlackUserID: field(c.SlackUserID), Phone: field(c.Phone)}
}

// getMyContact serves GET /me/contact: the caller's stored contact. Tenant
// and user come from the token only.
func (h *handlers) getMyContact(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	c, err := h.contacts.Get(r.Context(), id.TenantID, id.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toContactJSON(c))
}

// putMyContact serves PUT /me/contact (AP-05): the caller changes their own
// contact, which Taskify stores and forwards to AgentPulse through the
// outbox. There is no user in the path or body, so nobody can set another
// person's contact.
func (h *handlers) putMyContact(w http.ResponseWriter, r *http.Request) {
	var req contactRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	id := identity(r)
	c, err := h.contacts.Update(r.Context(), id.TenantID, id.UserID, req.patch())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toContactJSON(c))
}
