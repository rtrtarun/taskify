package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// SchedulerConfig tunes the deadline scheduler.
type SchedulerConfig struct {
	Interval          time.Duration // default 1m
	BatchSize         int           // tasks per transaction; default 100
	PublicBaseURL     string
	OutboxMaxAttempts int // default 10
	Now               func() time.Time
	Logger            *slog.Logger
	// Wake is called after a tick that wrote outbox rows. Optional.
	Wake func()
	// Jobs receives the blocker investigations of stale tasks. Nil turns the
	// stale scan off.
	Jobs repository.AgentJobRepository
	// StaleAfter is how long an in_progress task may go without a comment
	// or an edit before it is investigated; default domain.StaleAfter (G12).
	StaleAfter time.Duration
}

// Scheduler writes task.due_soon and task.overdue events, and queues
// blocker_investigator jobs for stale tasks (section 3.3).
//
// Replicas may run it at the same time: each batch claims its tasks with
// SKIP LOCKED and marks them in the same transaction, and the unique
// idempotency key stops a second row should two ever overlap. A stale task
// is marked by its job, whose dedup key names the day, so it is
// investigated at most once per day however many scans see it.
type Scheduler struct {
	cfg    SchedulerConfig
	tx     repository.TxManager
	tasks  repository.TaskRepository
	outbox repository.OutboxRepository
}

// NewScheduler returns a scheduler.
func NewScheduler(tx repository.TxManager, tasks repository.TaskRepository, outbox repository.OutboxRepository, cfg SchedulerConfig) *Scheduler {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.OutboxMaxAttempts <= 0 {
		cfg.OutboxMaxAttempts = 10
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Wake == nil {
		cfg.Wake = func() {}
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = domain.StaleAfter
	}
	return &Scheduler{cfg: cfg, tx: tx, tasks: tasks, outbox: outbox}
}

// Run ticks immediately and then every Interval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Error("deadline scheduler: tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Tick runs the reminder scans and then the stale scan to completion, and
// returns how many reminders it wrote. Stale investigations are agent jobs,
// not reminders, and are not counted.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	now := s.cfg.Now().UTC().Truncate(time.Microsecond)
	total := 0
	for _, scan := range []func(context.Context, time.Time) (int, error){s.dueSoonBatch, s.overdueBatch} {
		n, err := s.drain(ctx, now, scan)
		total += n
		if err != nil {
			return total, err
		}
	}
	if total > 0 {
		s.cfg.Wake()
	}
	if s.cfg.Jobs != nil {
		n, err := s.drain(ctx, now, s.staleBatch)
		if err != nil {
			return total, err
		}
		if n > 0 {
			s.cfg.Logger.Info("deadline scheduler: queued stale task investigations", "tasks", n)
		}
	}
	return total, nil
}

// drain runs scan until a batch comes back short.
func (s *Scheduler) drain(ctx context.Context, now time.Time, scan func(context.Context, time.Time) (int, error)) (int, error) {
	total := 0
	for {
		n, err := scan(ctx, now)
		total += n
		if err != nil || n < s.cfg.BatchSize {
			return total, err
		}
	}
}

// staleBatch queues one blocker investigation per claimed stale task (G12)
// and returns how many tasks it claimed. Agents and settings are checked by
// the claim, as the task transaction checks them for a change.
func (s *Scheduler) staleBatch(ctx context.Context, now time.Time) (int, error) {
	claimed := 0
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		tasks, err := s.tasks.ClaimStale(ctx, now.Add(-s.cfg.StaleAfter), now, s.cfg.BatchSize)
		if err != nil {
			return err
		}
		claimed = len(tasks)
		for _, t := range tasks {
			job, err := domain.NewStaleJob(t, now)
			if err != nil {
				return err
			}
			if _, err := s.cfg.Jobs.Insert(ctx, job); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return claimed, nil
}

// dueSoonBatch handles one batch and returns how many tasks it claimed.
func (s *Scheduler) dueSoonBatch(ctx context.Context, now time.Time) (int, error) {
	return s.batch(ctx, now, s.tasks.ClaimDueSoon, s.tasks.MarkDueSoonNotified, func(t *domain.Task) domain.Event {
		window := domain.DueWindow24h
		if t.DueAt.Sub(now) <= time.Hour {
			window = domain.DueWindow1h
		}
		return domain.NewDueSoonEvent(t, window, now)
	})
}

func (s *Scheduler) overdueBatch(ctx context.Context, now time.Time) (int, error) {
	return s.batch(ctx, now, s.tasks.ClaimOverdue, s.tasks.MarkOverdueNotified, func(t *domain.Task) domain.Event {
		return domain.NewOverdueEvent(t, now)
	})
}

func (s *Scheduler) batch(
	ctx context.Context, now time.Time,
	claim func(context.Context, time.Time, int) ([]*domain.Task, error),
	mark func(context.Context, string, uuid.UUID, time.Time) error,
	event func(*domain.Task) domain.Event,
) (int, error) {
	claimed := 0
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		tasks, err := claim(ctx, now, s.cfg.BatchSize)
		if err != nil {
			return err
		}
		claimed = len(tasks)
		var events []domain.Event
		for _, t := range tasks {
			events = append(events, event(t))
			if err := mark(ctx, t.TenantID, t.ID, now); err != nil {
				return err
			}
		}
		entries, err := domain.NewOutboxEntries(events, s.cfg.PublicBaseURL, s.cfg.OutboxMaxAttempts, now)
		if err != nil {
			return err
		}
		_, err = s.outbox.Insert(ctx, entries)
		return err
	})
	if err != nil {
		return 0, err
	}
	return claimed, nil
}
