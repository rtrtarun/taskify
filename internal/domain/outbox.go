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
func NewOutboxEntries(events []Event, publicBaseURL string, maxAttempts int, now time.Time) ([]OutboxEntry, error) {
	now = now.UTC()
	var out []OutboxEntry
	for _, e := range events {
		for _, n := range e.Notifications(publicBaseURL) {
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
				TenantID:       e.Task.TenantID,
				AggregateID:    e.Task.ID,
				EventType:      e.Type,
				IdempotencyKey: n.IdempotencyKey,
				Payload:        body,
				TraceContext:   map[string]string{},
				Status:         OutboxPending,
				MaxAttempts:    maxAttempts,
				NextAttemptAt:  now,
				CreatedAt:      now,
			})
		}
	}
	return out, nil
}
