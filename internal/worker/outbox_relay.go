// Package worker holds Taskify's background loops: the outbox relay, the
// deadline scheduler, and outbox cleanup. Each coordinates with other
// replicas through PostgreSQL row locks only, so any number may run.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

// Notifier delivers outbox rows to AgentPulse. *agentpulse.Client implements
// it.
type Notifier interface {
	// Notify sends one /v1/notify body.
	Notify(ctx context.Context, body json.RawMessage) error
	// PutContact replaces a user's contact (contact.update rows).
	PutContact(ctx context.Context, tenantID, userID string, c agentpulse.Contact) error
	// SubmitLearningSuggestion submits a learning suggestion
	// (learning.suggestion rows).
	SubmitLearningSuggestion(ctx context.Context, s agentpulse.LearningSuggestion) (string, error)
}

// RelayConfig tunes the relay. Zero values take the documented defaults.
type RelayConfig struct {
	Workers      int           // concurrent sends; default 8
	BatchSize    int           // rows per claim; default 100
	PollInterval time.Duration // default 200ms
	Lease        time.Duration // a claim older than this is reclaimed; default 60s
	// OpenCircuitDelay is how long a row waits when the breaker rejected it.
	// Default 10s, the breaker's cooldown.
	OpenCircuitDelay time.Duration
	// Backoff returns the delay before retry number attempts. Default: full
	// jitter, random(0, min(60s, 500ms * 2^attempts)).
	Backoff func(attempts int) time.Duration
	Now     func() time.Time
	Logger  *slog.Logger
	// Metrics may be nil.
	Metrics *observability.Metrics
}

func (c *RelayConfig) defaults() {
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 200 * time.Millisecond
	}
	if c.Lease <= 0 {
		c.Lease = 60 * time.Second
	}
	if c.OpenCircuitDelay <= 0 {
		c.OpenCircuitDelay = 10 * time.Second
	}
	if c.Backoff == nil {
		c.Backoff = FullJitter
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// FullJitter is the default backoff: random(0, min(60s, 500ms * 2^attempts)).
func FullJitter(attempts int) time.Duration { return FullJitterUpTo(60 * time.Second)(attempts) }

// FullJitterUpTo is FullJitter with another ceiling, OUTBOX_RETRY_MAX_DELAY.
// A conformance target lowers it so ten failed sends take seconds, not
// minutes. A ceiling of zero or less means 60s.
func FullJitterUpTo(ceiling time.Duration) func(attempts int) time.Duration {
	if ceiling <= 0 {
		ceiling = 60 * time.Second
	}
	return func(attempts int) time.Duration {
		const base = 500 * time.Millisecond
		limit := ceiling
		if attempts < 30 { // beyond, the shift overflows
			limit = min(ceiling, base<<attempts)
		}
		return rand.N(limit + 1)
	}
}

// Relay delivers outbox rows to AgentPulse.
type Relay struct {
	cfg      RelayConfig
	repo     repository.OutboxRepository
	notifier Notifier
	wake     chan struct{}
}

// NewRelay returns a relay.
func NewRelay(repo repository.OutboxRepository, n Notifier, cfg RelayConfig) *Relay {
	cfg.defaults()
	return &Relay{cfg: cfg, repo: repo, notifier: n, wake: make(chan struct{}, 1)}
}

// Wake asks the relay to poll now. It never blocks.
func (r *Relay) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run polls until ctx is cancelled. Sends already in flight when ctx is
// cancelled are allowed to finish and record their result.
func (r *Relay) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.reapLeases(ctx)
	}()
	defer wg.Wait()

	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-r.wake:
		}
		// Keep draining while batches come back full.
		for ctx.Err() == nil {
			n, err := r.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				r.cfg.Logger.Error("outbox relay: claim failed", "error", err)
			}
			if n < r.cfg.BatchSize {
				break
			}
		}
	}
}

// RunOnce claims one batch and sends it, returning the number of rows claimed.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	batch, err := r.repo.ClaimBatch(ctx, r.cfg.Now(), r.cfg.BatchSize)
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	// Results are recorded even during shutdown, so a send that reached
	// AgentPulse is not repeated after a restart.
	sendCtx := context.WithoutCancel(ctx)
	sem := make(chan struct{}, r.cfg.Workers)
	var wg sync.WaitGroup
	for _, e := range batch {
		sem <- struct{}{}
		wg.Add(1)
		go func(e domain.OutboxEntry) {
			defer func() { <-sem; wg.Done() }()
			r.dispatch(sendCtx, e)
		}(e)
	}
	wg.Wait()
	return len(batch), nil
}

func (r *Relay) dispatch(ctx context.Context, e domain.OutboxEntry) {
	// Continue the trace of the request that wrote the row.
	ctx = observability.ExtractTraceContext(ctx, e.TraceContext)
	ctx, span := observability.Tracer().Start(ctx, "outbox.dispatch", trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attribute.String("taskify.event_type", e.EventType), attribute.Int("taskify.attempt", e.Attempts+1)))
	defer span.End()

	start := time.Now()
	sendErr := r.send(ctx, e)
	elapsed := time.Since(start)
	now := r.cfg.Now()
	log := r.cfg.Logger.With("outbox_id", e.ID, "event_type", e.EventType, "tenant_id", e.TenantID,
		"trace_id", observability.TraceID(ctx))

	// A contact row is coalesced (OutboxRepository.Coalesce): a newer
	// contact may have been folded into it while this send was in flight,
	// and must then be sent too, never dropped with this one.
	coalesced := e.EventType == domain.EventContactUpdate
	requeued := false
	deadLetter := func(attempts int, msg string) error {
		if coalesced {
			var err error
			requeued, err = r.repo.MarkDeadLetterIfUnchanged(ctx, e.ID, *e.ClaimedAt, e.Payload, attempts, msg, now)
			return err
		}
		return r.repo.MarkDeadLetter(ctx, e.ID, *e.ClaimedAt, attempts, msg)
	}

	var err error
	outcome := observability.OutcomeDispatched
	switch {
	case sendErr == nil && coalesced:
		requeued, err = r.repo.MarkDispatchedIfUnchanged(ctx, e.ID, *e.ClaimedAt, e.Payload, now)
	case sendErr == nil:
		err = r.repo.MarkDispatched(ctx, e.ID, *e.ClaimedAt, now)
	case errors.Is(sendErr, agentpulse.ErrCircuitOpen):
		// AgentPulse was not contacted: this is not an attempt.
		outcome = observability.OutcomeCircuitOpen
		err = r.repo.Release(ctx, e.ID, *e.ClaimedAt, now.Add(r.cfg.OpenCircuitDelay))
	case errors.Is(sendErr, errMalformedRow) || !agentpulse.Retryable(sendErr):
		outcome = observability.OutcomeDeadLetter
		log.Error("outbox relay: rejected by AgentPulse, moved to dead letter", "error", sendErr)
		err = deadLetter(e.Attempts+1, sendErr.Error())
	default:
		attempts := e.Attempts + 1
		if attempts >= e.MaxAttempts {
			outcome = observability.OutcomeDeadLetter
			log.Error("outbox relay: attempts exhausted, moved to dead letter", "attempts", attempts, "error", sendErr)
			err = deadLetter(attempts, sendErr.Error())
		} else {
			outcome = observability.OutcomeRetry
			log.Warn("outbox relay: send failed, will retry", "attempts", attempts, "error", sendErr)
			err = r.repo.MarkRetry(ctx, e.ID, *e.ClaimedAt, attempts, now.Add(r.cfg.Backoff(attempts)), sendErr.Error())
		}
	}
	r.cfg.Metrics.ObserveDispatch(e.EventType, outcome, elapsed)
	span.SetAttributes(attribute.String("taskify.outcome", outcome))
	if sendErr != nil {
		span.RecordError(sendErr)
		span.SetStatus(codes.Error, outcome)
	}
	if err != nil {
		// The lease reaper returns the row to pending; AgentPulse
		// deduplicates the resend by idempotency key, and a repeated
		// contact replace changes nothing.
		log.Error("outbox relay: could not record result", "error", err)
	}
	if requeued {
		log.Info("outbox relay: a newer value arrived during the send; sending it next")
		r.Wake()
	}
}

// send delivers one row: a contact.update replaces the person's contact, a
// learning.suggestion submits a learning suggestion, and every other row is
// a /v1/notify body.
func (r *Relay) send(ctx context.Context, e domain.OutboxEntry) error {
	switch e.EventType {
	case domain.EventContactUpdate:
		var u domain.ContactUpdate
		if err := json.Unmarshal(e.Payload, &u); err != nil || u.UserID == "" {
			return errMalformedRow
		}
		return r.notifier.PutContact(ctx, e.TenantID, u.UserID,
			agentpulse.Contact{Email: u.Email, SlackUserID: u.SlackUserID, Phone: u.Phone})
	case domain.EventLearningSuggestion:
		// The payload is the request body; the row's tenant must be the
		// one it names.
		var s agentpulse.LearningSuggestion
		if err := json.Unmarshal(e.Payload, &s); err != nil || s.TenantID != e.TenantID {
			return errMalformedRow
		}
		_, err := r.notifier.SubmitLearningSuggestion(ctx, s)
		return err
	default:
		return r.notifier.Notify(ctx, e.Payload)
	}
}

// errMalformedRow is a row the relay cannot send. Retrying cannot help, so
// the row goes to dead letter at once.
var errMalformedRow = errors.New("outbox relay: the row's payload cannot be read")

// reapLeases periodically returns rows abandoned in processing to pending.
func (r *Relay) reapLeases(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Lease / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := r.repo.ReleaseExpiredLeases(ctx, r.cfg.Now().Add(-r.cfg.Lease))
			if err != nil && ctx.Err() == nil {
				r.cfg.Logger.Error("outbox relay: lease reaper failed", "error", err)
			} else if n > 0 {
				r.cfg.Logger.Warn("outbox relay: returned abandoned rows to pending", "rows", n)
			}
		}
	}
}
