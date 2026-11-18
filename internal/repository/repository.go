// Package repository defines the persistence interfaces. Implementations live
// in postgres (production) and memory (fast unit tests); the shared
// conformance suite in repotest runs against both.
//
// Transactions travel in the context: TxManager.WithTx hands fn a context,
// and every repository call made with that context joins the transaction.
// Calls that must run inside a transaction (row locks, claims) return
// ErrTxRequired when given a context without one.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// ErrTxRequired is returned by calls that lock rows outside a transaction.
var ErrTxRequired = errors.New("repository: call requires a transaction")

// TxManager runs a function in a transaction.
type TxManager interface {
	// WithTx commits when fn returns nil and rolls back otherwise. A WithTx
	// inside another joins the outer transaction.
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Cursor marks a position in a task list ordered by (updated_at, id)
// descending.
type Cursor struct {
	UpdatedAt time.Time
	ID        uuid.UUID
}

// TaskFilter selects tasks within one tenant. Empty fields match anything.
// Archived tasks are excluded unless Status is archived.
type TaskFilter struct {
	TenantID   string
	AssigneeID string
	Status     domain.Status
	Priority   domain.Priority
	Limit      int // must be positive
	After      *Cursor
}

// TaskPage is one page of a list. Next is nil on the last page.
type TaskPage struct {
	Tasks []*domain.Task
	Next  *Cursor
}

// TaskRepository stores tasks and their watchers. Every call is scoped to a
// tenant except the deadline scans, which run for the whole system.
type TaskRepository interface {
	Create(ctx context.Context, t *domain.Task) error
	// Get returns ErrNotFound for a task in another tenant.
	Get(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Task, error)
	// GetForUpdate locks the row until the transaction ends.
	GetForUpdate(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Task, error)
	// Update writes t if the stored version is expectedVersion, and returns
	// ErrVersionConflict otherwise. Watchers are replaced.
	Update(ctx context.Context, t *domain.Task, expectedVersion int) error
	List(ctx context.Context, f TaskFilter) (TaskPage, error)
	// Delete removes the task permanently, with its watchers and comments.
	// It returns ErrNotFound when no row was deleted.
	Delete(ctx context.Context, tenantID string, id uuid.UUID) error

	// ClaimDueSoon locks open tasks that need a due-soon reminder at now,
	// skipping rows another transaction holds.
	ClaimDueSoon(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error)
	// ClaimOverdue locks open, past-due tasks not yet reminded on now's UTC
	// date, skipping rows another transaction holds.
	ClaimOverdue(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error)
	// Reminder bookkeeping. These do not change the version: a reminder is
	// not an edit and must not cause a client's next save to conflict.
	MarkDueSoonNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error
	MarkOverdueNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error
}

// WorkspaceRepository stores workspaces.
type WorkspaceRepository interface {
	// Create returns ErrAlreadyExists for a duplicate (tenant, id).
	Create(ctx context.Context, w domain.Workspace) error
	Get(ctx context.Context, tenantID, id string) (domain.Workspace, error)
	// GetForUpdate locks the row until the transaction ends.
	GetForUpdate(ctx context.Context, tenantID, id string) (domain.Workspace, error)
	// Update writes the name and updated_at.
	Update(ctx context.Context, w domain.Workspace) error
}

// WorkspaceRuleRepository records the AgentPulse rules each workspace has.
type WorkspaceRuleRepository interface {
	List(ctx context.Context, tenantID, workspaceID string) ([]domain.WorkspaceRule, error)
	// Create returns ErrAlreadyExists for a duplicate key.
	Create(ctx context.Context, r domain.WorkspaceRule) error
}

// WebhookDeliveryRepository remembers accepted webhook callbacks.
type WebhookDeliveryRepository interface {
	// Record stores key and reports whether it was new. A false result means
	// the callback was already processed.
	Record(ctx context.Context, key string, at time.Time) (bool, error)
	DeleteBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// CommentRepository stores comments. Callers check the task's tenant first.
type CommentRepository interface {
	Create(ctx context.Context, c domain.Comment) error
	// ListRecent returns up to limit comments, newest first.
	ListRecent(ctx context.Context, taskID uuid.UUID, limit int) ([]domain.Comment, error)
}

// OutboxRepository stores notifications waiting for delivery.
//
// The Mark and Release calls take the ClaimedAt of the claim being settled
// and only change a row still held by that claim. A row that was reclaimed
// after its lease expired, or deleted by a permanent task delete, is left
// alone, and the call still succeeds.
type OutboxRepository interface {
	// Insert skips entries whose idempotency key already exists and returns
	// how many rows were written.
	Insert(ctx context.Context, entries []domain.OutboxEntry) (int, error)
	// ClaimBatch moves up to limit due pending rows to processing, setting
	// ClaimedAt to now. Concurrent callers never receive the same row.
	ClaimBatch(ctx context.Context, now time.Time, limit int) ([]domain.OutboxEntry, error)
	MarkDispatched(ctx context.Context, id uuid.UUID, claimedAt, at time.Time) error
	// MarkRetry returns the row to pending with a new attempt count.
	MarkRetry(ctx context.Context, id uuid.UUID, claimedAt time.Time, attempts int, next time.Time, lastErr string) error
	// Release returns the row to pending without counting an attempt, for
	// example when the circuit breaker rejected the call.
	Release(ctx context.Context, id uuid.UUID, claimedAt, next time.Time) error
	MarkDeadLetter(ctx context.Context, id uuid.UUID, claimedAt time.Time, attempts int, lastErr string) error
	// ReleaseExpiredLeases returns rows claimed before olderThan to pending.
	ReleaseExpiredLeases(ctx context.Context, olderThan time.Time) (int, error)
	// DeleteDispatchedBefore removes up to limit rows dispatched before cutoff.
	DeleteDispatchedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error)
	// Get returns one row by ID. Used by tests and, from M12, admin tools.
	Get(ctx context.Context, id uuid.UUID) (domain.OutboxEntry, error)
	// DeleteForAggregate removes every row for one task, in any status.
	DeleteForAggregate(ctx context.Context, tenantID string, aggregateID uuid.UUID) (int, error)
	// ListForAggregate returns every row for one task, oldest first.
	ListForAggregate(ctx context.Context, aggregateID uuid.UUID) ([]domain.OutboxEntry, error)
	CountByStatus(ctx context.Context, status domain.OutboxStatus) (int, error)
}
