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
	// Delete removes the task permanently, with its watchers, comments, and
	// dependency links in both directions.
	// It returns ErrNotFound when no row was deleted.
	Delete(ctx context.Context, tenantID string, id uuid.UUID) error

	// ClaimDueSoon locks open tasks that need a due-soon reminder at now,
	// skipping rows another transaction holds.
	ClaimDueSoon(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error)
	// ClaimOverdue locks open, past-due tasks not yet reminded on now's UTC
	// date, skipping rows another transaction holds.
	ClaimOverdue(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error)
	// ClaimStale locks up to limit in_progress tasks, across tenants, that
	// have had neither an edit (updated_at) nor a comment since staleBefore,
	// in workspaces whose settings allow the blocker investigator, and that
	// have no stale investigation queued for now's UTC date (the job dedup
	// key of domain.StaleJobDedupKey). Rows another transaction holds are
	// skipped. Oldest first.
	ClaimStale(ctx context.Context, staleBefore, now time.Time, limit int) ([]*domain.Task, error)
	// ListBriefRecipients returns up to limit people to brief on day (UTC),
	// across tenants, ordered by tenant and user: the assignees of todo and
	// in_progress tasks, and the owners (assignee, or creator when
	// unassigned) of tasks with a job awaiting approval, counting only
	// workspaces whose settings allow the digest agent. A person who already
	// has day's brief job (domain.BriefJobDedupKey) is left out. Each names
	// the first such workspace by ID. It takes no locks: the brief job's
	// unique dedup key stops a second brief if two scans overlap.
	ListBriefRecipients(ctx context.Context, day time.Time, limit int) ([]domain.BriefRecipient, error)
	// Reminder bookkeeping. These do not change the version: a reminder is
	// not an edit and must not cause a client's next save to conflict.
	MarkDueSoonNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error
	MarkOverdueNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error

	// CountAssigned counts the tenant's tasks assigned to assigneeID with the
	// given status and priority, in every workspace: the assignee's load as
	// the digest gatekeeper measures it (G14). It takes no locks.
	CountAssigned(ctx context.Context, tenantID, assigneeID string, status domain.Status, priority domain.Priority) (int, error)

	// WorkspaceLabels returns up to limit labels used in the workspace, most
	// used first, ties by name.
	WorkspaceLabels(ctx context.Context, tenantID, workspaceID string, limit int) ([]string, error)
	// SimilarOpen returns up to limit open tasks in the workspace whose
	// titles share words with text, best match first, never the task
	// exclude (default G18).
	SimilarOpen(ctx context.Context, tenantID, workspaceID string, exclude uuid.UUID, text string, limit int) ([]*domain.Task, error)
}

// WorkspaceRepository stores workspaces.
type WorkspaceRepository interface {
	// Create returns ErrAlreadyExists for a duplicate (tenant, id).
	Create(ctx context.Context, w domain.Workspace) error
	Get(ctx context.Context, tenantID, id string) (domain.Workspace, error)
	// GetForUpdate locks the row until the transaction ends.
	GetForUpdate(ctx context.Context, tenantID, id string) (domain.Workspace, error)
	// Update writes the name, agent settings, and updated_at.
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
	// RecordCallback stores an AgentPulse callbackId for the tenant and
	// reports whether it was new (AP-06). A concurrent insert of the same
	// pair waits for the first and then reports false.
	RecordCallback(ctx context.Context, tenantID, callbackID string, at time.Time) (bool, error)
	// CallbackSeen reports whether the tenant's callbackId is stored.
	CallbackSeen(ctx context.Context, tenantID, callbackID string) (bool, error)
	// DeleteCallbacksBefore removes callbackIds received before cutoff.
	DeleteCallbacksBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// ContactRepository stores people's contact details (AP-05), one row per
// tenant and user.
type ContactRepository interface {
	// Get returns the stored contact, or ErrNotFound.
	Get(ctx context.Context, tenantID, userID string) (domain.Contact, error)
	// GetForUpdate locks the person's contact until the transaction ends,
	// first creating an empty one if there is none, so that concurrent
	// changes to one person's contact run one after the other. It requires
	// a transaction.
	GetForUpdate(ctx context.Context, tenantID, userID string) (domain.Contact, error)
	// Save writes the contact's fields and updated_at.
	Save(ctx context.Context, c domain.Contact) error
}

// ErrJobMoved is returned by AgentJobRepository.Update when the job is no
// longer as the caller read it, because another worker or a recovery sweep
// changed it.
var ErrJobMoved = errors.New("repository: agent job is no longer in the expected status")

// AgentJobRepository stores agent_jobs and agent_run_history rows.
type AgentJobRepository interface {
	// Insert queues a job. A job whose dedup key is already stored is not an
	// error: Insert reports false and leaves the stored job alone.
	Insert(ctx context.Context, j domain.AgentJob) (bool, error)
	Get(ctx context.Context, tenantID string, id uuid.UUID) (*domain.AgentJob, error)
	// GetForUpdate requires a transaction.
	GetForUpdate(ctx context.Context, tenantID string, id uuid.UUID) (*domain.AgentJob, error)
	// Claim marks up to limit pending jobs of the given agent types running,
	// oldest first, skipping jobs another claimer holds, and sets their
	// updated_at to now. It spans tenants: the runtime serves all.
	Claim(ctx context.Context, now time.Time, limit int, agentTypes []string) ([]*domain.AgentJob, error)
	// Update saves the job's status, base version, results, error, attempts,
	// decision, and updated_at, provided the stored job still has status from
	// and updated_at fromUpdatedAt. updated_at is the job's lease: a worker
	// whose job was recovered and claimed again cannot overwrite it.
	// Otherwise Update returns ErrJobMoved, or domain.ErrNotFound when the job
	// is gone.
	Update(ctx context.Context, j *domain.AgentJob, from domain.JobStatus, fromUpdatedAt time.Time) error
	// RecoverStuck handles jobs left running since before olderThan by a
	// worker that died (default G11): each gets one more attempt and returns
	// to pending, or fails once it reaches maxAttempts.
	RecoverStuck(ctx context.Context, olderThan, now time.Time, maxAttempts int) (requeued, failed int, err error)
	// ListForTask returns the task's jobs, oldest first.
	ListForTask(ctx context.Context, tenantID string, taskID uuid.UUID) ([]*domain.AgentJob, error)
	// ListAwaitingApproval returns the tenant's jobs awaiting approval on a
	// task whose owner (assignee, or creator when unassigned) is userID: the
	// person the approval request went to. Oldest first.
	ListAwaitingApproval(ctx context.Context, tenantID, userID string) ([]*domain.AgentJob, error)
	// LatestBrief returns userID's newest morning brief in the tenant: the
	// most recently created completed digest job for them whose
	// execution_result holds a report. A brief skipped for having nothing to
	// report does not count. ErrNotFound when there is none.
	LatestBrief(ctx context.Context, tenantID, userID string) (*domain.AgentJob, error)
	InsertRun(ctx context.Context, r domain.AgentRun) error
	// ListRuns returns a job's turns in order.
	ListRuns(ctx context.Context, jobID uuid.UUID) ([]domain.AgentRun, error)
}

// DependencyRepository stores task_dependencies rows. Every call is scoped to
// a tenant.
type DependencyRepository interface {
	// LockTenant serialises dependency inserts within a tenant until the
	// transaction ends. Locking only the two tasks being linked is not enough:
	// two inserts on disjoint pairs can each pass the cycle check and together
	// close a cycle through existing links.
	LockTenant(ctx context.Context, tenantID string) error
	// Create inserts the link and reports whether it was new. An existing
	// link is left alone and is not an error.
	Create(ctx context.Context, d domain.Dependency) (bool, error)
	// Reaches reports whether to can be reached from from by following
	// dependencies (from depends on ... depends on to).
	Reaches(ctx context.Context, tenantID string, from, to uuid.UUID) (bool, error)
	// ListBlockers returns the tasks taskID depends on.
	ListBlockers(ctx context.Context, tenantID string, taskID uuid.UUID) ([]uuid.UUID, error)
	// ListDependents returns the tasks that depend on blockerID.
	ListDependents(ctx context.Context, tenantID string, blockerID uuid.UUID) ([]uuid.UUID, error)
	// HasOpenBlocker reports whether taskID depends on any task that is not
	// completed or archived.
	HasOpenBlocker(ctx context.Context, tenantID string, taskID uuid.UUID) (bool, error)
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
	// Coalesce keeps one row per idempotency key for state that AgentPulse
	// replaces whole, such as a person's contact, where only the latest
	// value matters. It inserts e, or folds e's payload (and trace) into
	// the row that has its key. A row that is pending, dispatched, or dead
	// returns to pending, due at e's NextAttemptAt, with no attempts and no
	// error. A row being sent keeps its claim and only takes the payload;
	// the relay sees the change when it settles the claim with
	// MarkDispatchedIfUnchanged or MarkDeadLetterIfUnchanged. So one key
	// is never sent by two workers at once, and every send reads the
	// latest payload.
	Coalesce(ctx context.Context, e domain.OutboxEntry) error
	// MarkDispatchedIfUnchanged settles a successful send of a coalesced
	// row: dispatched if the row still holds sent, or, when a newer payload
	// was folded in during the send, pending again, due at at, with no
	// attempts. requeued reports the latter.
	MarkDispatchedIfUnchanged(ctx context.Context, id uuid.UUID, claimedAt time.Time, sent []byte, at time.Time) (requeued bool, err error)
	// MarkDeadLetterIfUnchanged is MarkDeadLetter for a coalesced row: a
	// newer payload folded in during the failed send is not dead with it,
	// and returns to pending, due at at, with no attempts.
	MarkDeadLetterIfUnchanged(ctx context.Context, id uuid.UUID, claimedAt time.Time, sent []byte, attempts int, lastErr string, at time.Time) (requeued bool, err error)
	// DeleteForAggregate removes every row for one task, in any status.
	DeleteForAggregate(ctx context.Context, tenantID string, aggregateID uuid.UUID) (int, error)
	// ListForAggregate returns every row for one task, oldest first.
	ListForAggregate(ctx context.Context, aggregateID uuid.UUID) ([]domain.OutboxEntry, error)
	CountByStatus(ctx context.Context, status domain.OutboxStatus) (int, error)
}
