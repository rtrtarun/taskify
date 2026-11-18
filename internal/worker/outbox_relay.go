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

	"github.com/RONGALI-TARUN/taskify/internal/agentpulse"
	"github.com/RONGALI-TARUN/taskify/internal/domain"
	"github.com/RONGALI-TARUN/taskify/internal/repository"
)

// Notifier sends one /notify body. *agentpulse.Client implements it.
type Notifier interface {
	Notify(ctx context.Context, body json.RawMessage) error
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
func FullJitter(attempts int) time.Duration {
	const base, ceiling = 500 * time.Millisecond, 60 * time.Second
	limit := ceiling
	if attempts < 7 { // 500ms * 2^7 already exceeds 60s
		limit = min(ceiling, base<<attempts)
	}
	return rand.N(limit + 1)
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
	if tp := e.TraceContext["traceparent"]; tp != "" {
		ctx = agentpulse.WithTraceparent(ctx, tp)
	}
	sendErr := r.notifier.Notify(ctx, e.Payload)
	now := r.cfg.Now()
	log := r.cfg.Logger.With("outbox_id", e.ID, "event_type", e.EventType, "tenant_id", e.TenantID)

	var err error
	switch {
	case sendErr == nil:
		err = r.repo.MarkDispatched(ctx, e.ID, *e.ClaimedAt, now)
	case errors.Is(sendErr, agentpulse.ErrCircuitOpen):
		// AgentPulse was not contacted: this is not an attempt.
		err = r.repo.Release(ctx, e.ID, *e.ClaimedAt, now.Add(r.cfg.OpenCircuitDelay))
	case !agentpulse.Retryable(sendErr):
		log.Error("outbox relay: rejected by AgentPulse, moved to dead letter", "error", sendErr)
		err = r.repo.MarkDeadLetter(ctx, e.ID, *e.ClaimedAt, e.Attempts+1, sendErr.Error())
	default:
		attempts := e.Attempts + 1
		if attempts >= e.MaxAttempts {
			log.Error("outbox relay: attempts exhausted, moved to dead letter", "attempts", attempts, "error", sendErr)
			err = r.repo.MarkDeadLetter(ctx, e.ID, *e.ClaimedAt, attempts, sendErr.Error())
		} else {
			log.Warn("outbox relay: send failed, will retry", "attempts", attempts, "error", sendErr)
			err = r.repo.MarkRetry(ctx, e.ID, *e.ClaimedAt, attempts, now.Add(r.cfg.Backoff(attempts)), sendErr.Error())
		}
	}
	if err != nil {
		// The lease reaper returns the row to pending; AgentPulse
		// deduplicates the resend by idempotency key.
		log.Error("outbox relay: could not record result", "error", err)
	}
}

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
