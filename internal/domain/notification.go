package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// EventFeedbackForward is the outbox event type that forwards a person's
// reaction to an in-app notification to AgentPulse (M14-06, G20). The relay
// sends it to POST /v1/feedback instead of /v1/notify.
const EventFeedbackForward = "feedback.forward"

// Reactions a person can record on an in-app notification.
const (
	AckActed     = "acted"
	AckDismissed = "dismissed"
	AckSnoozed   = "snoozed"
)

// MaxNotificationIDLen bounds an AgentPulse notification ID.
const MaxNotificationIDLen = 128

// ValidateNotificationID accepts a visible-ASCII ID of 1 to 128 characters,
// the form of every ID AgentPulse has issued. It never reaches a query as
// anything but a bound value.
func ValidateNotificationID(id string) error {
	if id == "" || len(id) > MaxNotificationIDLen {
		return fmt.Errorf("%w: notification id must be 1 to %d characters", ErrValidation, MaxNotificationIDLen)
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= 0x20 || id[i] >= 0x7f {
			return fmt.Errorf("%w: notification id must be printable ASCII without spaces", ErrValidation)
		}
	}
	return nil
}

// NotificationAck is one person's reaction to one notification. There is at
// most one per tenant, user, and notification: a later reaction replaces it.
type NotificationAck struct {
	TenantID       string
	UserID         string
	NotificationID string
	Signal         string
	// SnoozeDuration and SnoozeUntil are set for AckSnoozed only.
	SnoozeDuration string
	SnoozeUntil    *time.Time
	AckedAt        time.Time
}

// FeedbackForward is the payload of a feedback.forward outbox row. The tenant
// is the row's.
type FeedbackForward struct {
	UserID         string `json:"userId"`
	NotificationID string `json:"notificationId"`
	Signal         string `json:"signal"`
	SnoozeDuration string `json:"snoozeDuration,omitempty"`
}

var feedbackNamespace = uuid.MustParse("3b0e8a52-9d5e-4c1b-8a57-5f7f4f0f6a11")

// FeedbackAggregateID is the outbox aggregate ID of a person's feedback on a
// notification: a name-based UUID, so it never names a task or a job.
func FeedbackAggregateID(tenantID, userID, notificationID string) uuid.UUID {
	return uuid.NewSHA1(feedbackNamespace, []byte(tenantID+"\x00"+userID+"\x00"+notificationID))
}

// FeedbackIdempotencyKey names one forward of one reaction. It carries the
// time of the reaction, so reacting a second, different way (or changing back)
// is a new forward, while one reaction is never forwarded twice. Tenant IDs
// cannot hold a colon, so the key is unambiguous up to the user and
// notification, which may.
func FeedbackIdempotencyKey(a NotificationAck) string {
	return Source + ":" + a.TenantID + ":" + EventFeedbackForward + ":" + strconv.FormatInt(a.AckedAt.UnixMicro(), 10) +
		":" + a.UserID + ":" + a.NotificationID
}

// NewFeedbackEntry returns the outbox row that forwards the reaction.
func NewFeedbackEntry(a NotificationAck, maxAttempts int) (OutboxEntry, error) {
	if err := ValidateTenantID(a.TenantID); err != nil {
		return OutboxEntry{}, err
	}
	if err := ValidateUserID("user_id", a.UserID); err != nil {
		return OutboxEntry{}, err
	}
	if err := ValidateNotificationID(a.NotificationID); err != nil {
		return OutboxEntry{}, err
	}
	body, err := json.Marshal(FeedbackForward{UserID: a.UserID, NotificationID: a.NotificationID, Signal: a.Signal, SnoozeDuration: a.SnoozeDuration})
	if err != nil {
		return OutboxEntry{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return OutboxEntry{}, err
	}
	at := a.AckedAt.UTC()
	return OutboxEntry{
		ID:             id,
		TenantID:       a.TenantID,
		AggregateID:    FeedbackAggregateID(a.TenantID, a.UserID, a.NotificationID),
		EventType:      EventFeedbackForward,
		IdempotencyKey: FeedbackIdempotencyKey(a),
		Payload:        body,
		TraceContext:   map[string]string{},
		Status:         OutboxPending,
		MaxAttempts:    maxAttempts,
		NextAttemptAt:  at,
		CreatedAt:      at,
	}, nil
}
