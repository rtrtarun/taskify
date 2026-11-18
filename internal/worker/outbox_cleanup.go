package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/RONGALI-TARUN/taskify/internal/repository"
)

// CleanupConfig tunes outbox cleanup.
type CleanupConfig struct {
	Interval  time.Duration // default 24h
	Retention time.Duration // dispatched rows older than this are deleted; default 7 days
	BatchSize int           // rows per delete; default 5000
	// Deliveries, if set, also has webhook delivery records older than
	// DeliveryRetention (default 1h) deleted.
	Deliveries        repository.WebhookDeliveryRepository
	DeliveryRetention time.Duration
	Now               func() time.Time
	Logger            *slog.Logger
}

// Cleanup deletes dispatched outbox rows past their retention, in batches, so
// the table and its partial index stay small. Pending and dead-letter rows are
// never deleted.
type Cleanup struct {
	cfg    CleanupConfig
	outbox repository.OutboxRepository
}

// NewCleanup returns a cleanup worker.
func NewCleanup(outbox repository.OutboxRepository, cfg CleanupConfig) *Cleanup {
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 5000
	}
	if cfg.DeliveryRetention <= 0 {
		cfg.DeliveryRetention = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Cleanup{cfg: cfg, outbox: outbox}
}

// Run cleans up immediately and then every Interval until ctx is cancelled.
func (c *Cleanup) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		if n, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			c.cfg.Logger.Error("outbox cleanup failed", "error", err)
		} else if n > 0 {
			c.cfg.Logger.Info("outbox cleanup", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce deletes every expired row and returns how many it deleted.
func (c *Cleanup) RunOnce(ctx context.Context) (int, error) {
	total := 0
	if c.cfg.Deliveries != nil {
		n, err := c.cfg.Deliveries.DeleteBefore(ctx, c.cfg.Now().Add(-c.cfg.DeliveryRetention))
		if err != nil {
			return 0, err
		}
		total += n
	}
	cutoff := c.cfg.Now().Add(-c.cfg.Retention)
	for ctx.Err() == nil {
		n, err := c.outbox.DeleteDispatchedBefore(ctx, cutoff, c.cfg.BatchSize)
		total += n
		if err != nil || n < c.cfg.BatchSize {
			return total, err
		}
	}
	return total, ctx.Err()
}
