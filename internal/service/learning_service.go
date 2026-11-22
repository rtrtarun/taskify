package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

// LearningService submits the learning agent's suggestions to AgentPulse
// (M11-03). Like a task write, it changes PostgreSQL only: the suggestion's
// record and its learning.suggestion outbox row commit together, and the
// relay makes the call. AgentPulse holds the suggestion as PENDING_APPROVAL,
// so a submission changes nothing that notifies anyone; an admin decides it
// through the learning-suggestion endpoints.
type LearningService struct {
	tx          repository.TxManager
	suggestions repository.LearningSuggestionRepository
	outbox      repository.OutboxRepository
	maxAttempts int
	now         func() time.Time
	wake        func()
}

// NewLearningService returns a LearningService. now defaults to time.Now,
// and wake, called after a submission commits, may be nil.
func NewLearningService(tx repository.TxManager, suggestions repository.LearningSuggestionRepository,
	outbox repository.OutboxRepository, maxAttempts int, now func() time.Time, wake func()) *LearningService {
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	if now == nil {
		now = time.Now
	}
	if wake == nil {
		wake = func() {}
	}
	return &LearningService{tx: tx, suggestions: suggestions, outbox: outbox, maxAttempts: maxAttempts, now: now, wake: wake}
}

// Submit records the suggestion and queues it for AgentPulse, in one
// transaction, and reports whether it was new. A job that already submitted
// a suggestion for the pattern, as a retried run may try to, submits nothing
// more, so AgentPulse, which takes no idempotency key for suggestions, is
// asked once. The suggestion is validated first: an adjustment outside -20
// to 20 is ErrValidation and nothing is written.
func (s *LearningService) Submit(ctx context.Context, sg domain.LearningSuggestion) (bool, error) {
	now := s.now().UTC().Truncate(time.Microsecond)
	sg.CreatedAt = now
	if sg.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return false, err
		}
		sg.ID = id
	}
	entry, err := domain.NewLearningSuggestionEntry(sg, s.maxAttempts, now)
	if err != nil {
		return false, err
	}
	entry.TraceContext = observability.InjectTraceContext(ctx)
	created := false
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		created = false // a retried transaction starts again
		fresh, err := s.suggestions.Insert(ctx, sg)
		if err != nil || !fresh {
			return err
		}
		if _, err := s.outbox.Insert(ctx, []domain.OutboxEntry{entry}); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if created {
		s.wake()
	}
	return created, nil
}
