package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

// In-app notification limits.
const (
	DefaultFeedLimit = 20
	MaxFeedLimit     = 100
	maxFeedCursorLen = 512
	// feedTimeout is how long the feed read waits for AgentPulse. It is a
	// read on the user's page load, so it is short.
	feedTimeout = 3 * time.Second
)

// ErrFeedUnavailable is returned when AgentPulse cannot give the feed now: it
// did not answer in time, its circuit breaker is open, it is unreachable, or
// it answered with a server error. The caller answers 503 with a retry hint;
// no task read or write depends on it.
var ErrFeedUnavailable = errors.New("the notification feed is unavailable")

// FeedSource reads a user's in-app feed from AgentPulse. *agentpulse.Client
// implements it.
type FeedSource interface {
	InAppFeed(ctx context.Context, tenantID, userID string, limit int, cursor string) (json.RawMessage, error)
}

// NotificationService serves a person's in-app notifications (M14-06).
//
// Who is asking. Every method takes the tenant and user as parameters, and
// the HTTP handler passes only the ones from the verified token. Nothing in a
// request can name another person: the feed is fetched for that pair, and
// acks are keyed by it.
//
// Like a task write, an ack changes PostgreSQL only: the ack and the outbox
// row that forwards its feedback signal commit together, and the relay makes
// the call to AgentPulse, retrying it while AgentPulse is down.
type NotificationService struct {
	tx          repository.TxManager
	feed        FeedSource
	tasks       repository.TaskRepository
	acks        repository.NotificationAckRepository
	outbox      repository.OutboxRepository
	maxAttempts int
	now         func() time.Time
	wake        func()
	metrics     *observability.Metrics
}

// NewNotificationService returns a NotificationService. now and wake (called
// after an ack commits) may be nil; so may metrics.
func NewNotificationService(tx repository.TxManager, feed FeedSource, tasks repository.TaskRepository,
	acks repository.NotificationAckRepository, outbox repository.OutboxRepository,
	maxAttempts int, now func() time.Time, wake func(), metrics *observability.Metrics) *NotificationService {
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	if now == nil {
		now = time.Now
	}
	if wake == nil {
		wake = func() {}
	}
	return &NotificationService{tx: tx, feed: feed, tasks: tasks, acks: acks, outbox: outbox,
		maxAttempts: maxAttempts, now: now, wake: wake, metrics: metrics}
}

// FeedItem is one notification with what Taskify adds to it.
type FeedItem struct {
	ID        string
	EventType string
	Title     string
	Body      string
	CreatedAt time.Time
	// TaskID and JobID are what the notification's payload names, if valid.
	TaskID *uuid.UUID
	JobID  *uuid.UUID
	// Task is the named task, when it is in the caller's tenant.
	Task *domain.Task
	// Ack is the caller's reaction, or nil.
	Ack *domain.NotificationAck
}

// FeedPage is one page of the feed. Next is empty on the last page.
type FeedPage struct {
	Items []FeedItem
	Next  string
}

// apNotification and apFeed are AgentPulse's bodies (docs/blockers.md, R8).
// They stay in this file and the agentpulse client.
type apNotification struct {
	ID        string          `json:"id"`
	EventType string          `json:"eventType"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	CreatedAt string          `json:"createdAt"`
	Payload   json.RawMessage `json:"payload"`
}

type apFeed struct {
	Notifications []apNotification `json:"notifications"`
	NextCursor    *string          `json:"nextCursor"`
}

// Feed returns one page of the person's in-app feed, newest first, with each
// item's task and the person's own read state. limit 0 means DefaultFeedLimit.
// It returns ErrFeedUnavailable when AgentPulse cannot answer; a malformed
// item is skipped, and a malformed page is an upstream failure.
func (s *NotificationService) Feed(ctx context.Context, tenantID, userID string, limit int, cursor string) (FeedPage, error) {
	if err := domain.ValidateTenantID(tenantID); err != nil {
		return FeedPage{}, err
	}
	if err := domain.ValidateUserID("user_id", userID); err != nil {
		return FeedPage{}, err
	}
	switch {
	case limit == 0:
		limit = DefaultFeedLimit
	case limit < 0 || limit > MaxFeedLimit:
		return FeedPage{}, fmt.Errorf("%w: limit must be 1 to %d", domain.ErrValidation, MaxFeedLimit)
	}
	if len(cursor) > maxFeedCursorLen {
		return FeedPage{}, fmt.Errorf("%w: cursor is too long", domain.ErrValidation)
	}

	fctx, cancel := context.WithTimeout(ctx, feedTimeout)
	defer cancel()
	raw, err := s.feed.InAppFeed(fctx, tenantID, userID, limit, cursor)
	if err != nil {
		return FeedPage{}, classifyFeedError(ctx, err)
	}
	var in apFeed
	if err := json.Unmarshal(raw, &in); err != nil {
		return FeedPage{}, fmt.Errorf("%w: AgentPulse sent a feed Taskify cannot read", ErrFeedUnavailable)
	}

	page := FeedPage{Items: make([]FeedItem, 0, len(in.Notifications))}
	if in.NextCursor != nil {
		page.Next = *in.NextCursor
	}
	ids := make([]string, 0, len(in.Notifications))
	taskIDs := map[uuid.UUID]*domain.Task{}
	for _, n := range in.Notifications {
		created, err := time.Parse(time.RFC3339, n.CreatedAt)
		if n.ID == "" || domain.ValidateNotificationID(n.ID) != nil || err != nil {
			continue // not something the UI can act on
		}
		item := FeedItem{ID: n.ID, EventType: n.EventType, Title: n.Title, Body: n.Body, CreatedAt: created.UTC()}
		var p struct {
			TaskID string `json:"taskId"`
			JobID  string `json:"jobId"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if id, err := uuid.Parse(p.TaskID); err == nil {
			item.TaskID = &id
			taskIDs[id] = nil
		}
		if id, err := uuid.Parse(p.JobID); err == nil {
			item.JobID = &id
		}
		ids = append(ids, n.ID)
		page.Items = append(page.Items, item)
	}

	// The tasks the notifications name, looked up in the caller's tenant:
	// a task of another tenant is not found, so its data never leaks.
	for id := range taskIDs {
		t, err := s.tasks.Get(ctx, tenantID, id)
		switch {
		case err == nil:
			taskIDs[id] = t
		case errors.Is(err, domain.ErrNotFound):
		default:
			return FeedPage{}, err
		}
	}
	acks, err := s.acks.ListFor(ctx, tenantID, userID, ids)
	if err != nil {
		return FeedPage{}, err
	}
	for i := range page.Items {
		it := &page.Items[i]
		if it.TaskID != nil {
			it.Task = taskIDs[*it.TaskID]
		}
		if a, ok := acks[it.ID]; ok {
			a := a
			it.Ack = &a
		}
	}
	return page, nil
}

// classifyFeedError separates "AgentPulse cannot answer now" from everything
// else. Only a cancelled caller is its own error.
func classifyFeedError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if isUpstreamServerFailure(err) {
		return fmt.Errorf("%w: %w", ErrFeedUnavailable, err)
	}
	return err
}

// AckInput is a person's reaction to a notification.
type AckInput struct {
	TenantID       string
	UserID         string
	NotificationID string
	Signal         string // acted, dismissed, or snoozed
	Duration       string // snoozed only, such as "2h"
}

// Ack records the reaction and queues one feedback signal for AgentPulse.
//
// The ack and the outbox row commit in one transaction, so there is never an
// ack without its forward or a forward without its ack, and the click does
// not depend on AgentPulse. The relay sends the row and retries it on
// failure: a failed first attempt is not a second signal, because the row,
// not the attempt, is the unit that is sent until it succeeds.
//
// The save is a single upsert that reports whether it changed anything, so
// the same reaction twice, on one replica or two at once, queues one row.
// A different reaction replaces the first and queues its own.
func (s *NotificationService) Ack(ctx context.Context, in AckInput) (domain.NotificationAck, error) {
	if err := domain.ValidateTenantID(in.TenantID); err != nil {
		return domain.NotificationAck{}, err
	}
	if err := domain.ValidateUserID("user_id", in.UserID); err != nil {
		return domain.NotificationAck{}, err
	}
	if err := domain.ValidateNotificationID(in.NotificationID); err != nil {
		return domain.NotificationAck{}, err
	}
	if in.Signal != domain.AckActed && in.Signal != domain.AckDismissed && in.Signal != domain.AckSnoozed {
		return domain.NotificationAck{}, fmt.Errorf("%w: signal must be acted, dismissed, or snoozed", domain.ErrValidation)
	}
	// ParseSignal also refuses a duration on any signal but snoozed.
	snooze, err := ParseSignal(in.Signal, in.Duration)
	if err != nil {
		return domain.NotificationAck{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	ack := domain.NotificationAck{TenantID: in.TenantID, UserID: in.UserID, NotificationID: in.NotificationID, Signal: in.Signal, AckedAt: now}
	if in.Signal == domain.AckSnoozed {
		ack.SnoozeDuration = strings.TrimSpace(in.Duration)
		until := now.Add(snooze)
		ack.SnoozeUntil = &until
	}

	var saved domain.NotificationAck
	var queued bool
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		changed, err := s.acks.Save(ctx, ack)
		if err != nil {
			return err
		}
		if !changed {
			// The reaction is already recorded, and already queued. Report
			// the stored one, whose time and snooze end the first call set.
			got, err := s.acks.ListFor(ctx, in.TenantID, in.UserID, []string{in.NotificationID})
			if err != nil {
				return err
			}
			saved = got[in.NotificationID]
			return nil
		}
		saved = ack
		entry, err := domain.NewFeedbackEntry(ack, s.maxAttempts)
		if err != nil {
			return err
		}
		entry.TraceContext = observability.InjectTraceContext(ctx)
		if _, err := s.outbox.Insert(ctx, []domain.OutboxEntry{entry}); err != nil {
			return err
		}
		queued = true
		return nil
	})
	if err != nil {
		return domain.NotificationAck{}, err
	}
	if queued {
		s.wake()
	}
	return saved, nil
}

// isUpstreamServerFailure reports whether err means AgentPulse could not
// answer: a timeout, an unreachable host, an open breaker, a 5xx, or 429. A
// 4xx other than 429 means AgentPulse answered and refused, which is not
// "try again shortly".
func isUpstreamServerFailure(err error) bool {
	var se *agentpulse.StatusError
	if errors.As(err, &se) {
		return se.Code >= 500 || se.Code == http.StatusTooManyRequests
	}
	return agentpulse.IsUpstream(err) || errors.Is(err, context.DeadlineExceeded)
}
