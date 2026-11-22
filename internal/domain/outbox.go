package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// OutboxStatus is the delivery state of an outbox row.
type OutboxStatus string

const (
	OutboxPending    OutboxStatus = "pending"
	OutboxProcessing OutboxStatus = "processing"
	OutboxDispatched OutboxStatus = "dispatched"
	OutboxDeadLetter OutboxStatus = "dead_letter"
)

// OutboxEntry is one notification waiting to be sent to AgentPulse. Its
// Payload is the complete /notify body, so the relay needs no knowledge of
// tasks or events.
type OutboxEntry struct {
	ID             uuid.UUID
	TenantID       string
	AggregateID    uuid.UUID
	EventType      string
	IdempotencyKey string
	Payload        json.RawMessage
	TraceContext   map[string]string
	Status         OutboxStatus
	Attempts       int
	MaxAttempts    int
	NextAttemptAt  time.Time
	// ClaimedAt is set while a relay worker holds the row. It doubles as a
	// fencing token: a worker may only record a result for the claim it made.
	ClaimedAt    *time.Time
	LastError    string
	CreatedAt    time.Time
	DispatchedAt *time.Time
}

// NewOutboxEntries turns events into pending outbox rows, one per event per
// recipient. An event with no recipients produces no rows.
func NewOutboxEntries(events []Event, links Links, maxAttempts int, now time.Time) ([]OutboxEntry, error) {
	var out []OutboxEntry
	for _, e := range events {
		entries, err := NewNotificationEntries(e.Notifications(links), e.Task.ID, maxAttempts, now)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	return out, nil
}

// NewNotificationEntries turns notifications into pending outbox rows.
// aggregateID is what they are about: the task for a task event, or the
// agent job for a notification that is about no single task, such as a
// morning brief.
func NewNotificationEntries(ns []Notification, aggregateID uuid.UUID, maxAttempts int, now time.Time) ([]OutboxEntry, error) {
	now = now.UTC()
	out := make([]OutboxEntry, 0, len(ns))
	for _, n := range ns {
		body, err := json.Marshal(n)
		if err != nil {
			return nil, err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		out = append(out, OutboxEntry{
			ID:             id,
			TenantID:       n.TenantID,
			AggregateID:    aggregateID,
			EventType:      n.EventType,
			IdempotencyKey: n.IdempotencyKey,
			Payload:        body,
			TraceContext:   map[string]string{},
			Status:         OutboxPending,
			MaxAttempts:    maxAttempts,
			NextAttemptAt:  now,
			CreatedAt:      now,
		})
	}
	return out, nil
}
