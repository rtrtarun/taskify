package domain

import (
	"encoding/json"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EventContactUpdate is the outbox event type that forwards a person's
// contact to AgentPulse (AP-05). The relay sends it to
// PUT /v1/tenants/{t}/users/{u}/contact instead of /v1/notify.
const EventContactUpdate = "contact.update"

// Contact formats, exactly as AgentPulse validates them (AP-12), so a value
// Taskify accepts is never refused: an email is a plain address of at most
// 254 characters with no display name; a Slack user ID is U or W and then
// capital letters and digits, at most 32 characters in all; a phone number
// is E.164, a plus sign and 2 to 15 digits, the first not 0.
const maxEmailLen = 254

var (
	slackUserIDRe = regexp.MustCompile(`^[UW][A-Z0-9]{1,31}$`)
	phoneRe       = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)
)

// Contact is how AgentPulse reaches a person on Slack, email, or phone. Each
// field is nil when unset. A person with no contact gets in-app and digest
// delivery only. Taskify keeps the latest contact per tenant and user and
// forwards it whole: AgentPulse replaces its copy on every forward.
type Contact struct {
	TenantID    string
	UserID      string
	Email       *string
	SlackUserID *string
	Phone       *string
	// UpdatedAt is zero for a person who never set a contact.
	UpdatedAt time.Time
}

// Empty reports whether no field is set.
func (c Contact) Empty() bool { return c.Email == nil && c.SlackUserID == nil && c.Phone == nil }

// ContactField changes one field of a contact: unset leaves it alone, Set
// with a nil Value clears it.
type ContactField struct {
	Set   bool
	Value *string
}

// ContactPatch is a person's change to their own contact.
type ContactPatch struct {
	Email       ContactField
	SlackUserID ContactField
	Phone       ContactField
}

// Apply returns the contact with the patch applied and whether anything
// changed. Values are trimmed, and an empty value clears the field like
// null. An invalid value fails the whole patch with ErrValidation.
func (c Contact) Apply(p ContactPatch, now time.Time) (Contact, bool, error) {
	next := c
	for _, f := range []struct {
		name  string
		patch ContactField
		dst   **string
		check func(string) error
	}{
		{"email", p.Email, &next.Email, checkEmail},
		{"slack_user_id", p.SlackUserID, &next.SlackUserID, checkSlackUserID},
		{"phone", p.Phone, &next.Phone, checkPhone},
	} {
		if !f.patch.Set {
			continue
		}
		if f.patch.Value == nil || strings.TrimSpace(*f.patch.Value) == "" {
			*f.dst = nil
			continue
		}
		v := strings.TrimSpace(*f.patch.Value)
		if err := f.check(v); err != nil {
			return c, false, err
		}
		*f.dst = &v
	}
	if equalStr(c.Email, next.Email) && equalStr(c.SlackUserID, next.SlackUserID) && equalStr(c.Phone, next.Phone) {
		return c, false, nil
	}
	next.UpdatedAt = now.UTC()
	return next, true, nil
}

func checkEmail(v string) error {
	a, err := mail.ParseAddress(v)
	if err != nil || a.Name != "" || a.Address != v || len(v) > maxEmailLen {
		return invalid("email must be a plain address such as name@example.com, at most %d characters", maxEmailLen)
	}
	return nil
}

func checkSlackUserID(v string) error {
	if !slackUserIDRe.MatchString(v) {
		return invalid("slack_user_id must be a Slack member ID such as U024BE7LH")
	}
	return nil
}

func checkPhone(v string) error {
	if !phoneRe.MatchString(v) {
		return invalid("phone must be in E.164 form, such as +14155550123")
	}
	return nil
}

func equalStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ContactUpdate is the payload of a contact.update outbox row: the person,
// and their whole contact with every field present, null when unset.
type ContactUpdate struct {
	UserID      string  `json:"userId"`
	Email       *string `json:"email"`
	SlackUserID *string `json:"slackUserId"`
	Phone       *string `json:"phone"`
}

// ContactIdempotencyKey is the key of a person's one contact.update row.
// It names no version: the row is reused for every change (see
// OutboxRepository.Coalesce), so at most one forward per person exists.
// Tenant IDs cannot hold a colon, so the key is unambiguous.
func ContactIdempotencyKey(tenantID, userID string) string {
	return Source + ":" + tenantID + ":" + EventContactUpdate + ":" + userID
}

// contactNamespace scopes ContactAggregateID.
var contactNamespace = uuid.MustParse("6f1c2c43-6a8e-4f0e-9a51-1f0f6c5e2b7d")

// ContactAggregateID is the outbox aggregate ID of a person's contact rows:
// a name-based UUID, so it never names a task or a job.
func ContactAggregateID(tenantID, userID string) uuid.UUID {
	return uuid.NewSHA1(contactNamespace, []byte(tenantID+"\x00"+userID))
}

// NewContactEntry returns the outbox row that forwards c to AgentPulse.
func NewContactEntry(c Contact, maxAttempts int, now time.Time) (OutboxEntry, error) {
	if err := ValidateTenantID(c.TenantID); err != nil {
		return OutboxEntry{}, err
	}
	if err := ValidateUserID("user_id", c.UserID); err != nil {
		return OutboxEntry{}, err
	}
	body, err := json.Marshal(ContactUpdate{UserID: c.UserID, Email: c.Email, SlackUserID: c.SlackUserID, Phone: c.Phone})
	if err != nil {
		return OutboxEntry{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return OutboxEntry{}, err
	}
	now = now.UTC()
	return OutboxEntry{
		ID:             id,
		TenantID:       c.TenantID,
		AggregateID:    ContactAggregateID(c.TenantID, c.UserID),
		EventType:      EventContactUpdate,
		IdempotencyKey: ContactIdempotencyKey(c.TenantID, c.UserID),
		Payload:        body,
		TraceContext:   map[string]string{},
		Status:         OutboxPending,
		MaxAttempts:    maxAttempts,
		NextAttemptAt:  now,
		CreatedAt:      now,
	}, nil
}
