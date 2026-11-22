package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// SchedulerConfig tunes the deadline scheduler.
type SchedulerConfig struct {
	Interval      time.Duration // default 1m
	BatchSize     int           // tasks per transaction; default 100
	PublicBaseURL string
	// AppBaseURL is the web app's address, or empty (AP-10).
	AppBaseURL        string
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
	// Briefs turns on the morning brief: from BriefHourUTC each day, one
	// digest job is queued per person with something to report (G15). It
	// needs Jobs.
	Briefs bool
	// BriefHourUTC is the hour, 0 to 23, at which briefs are due
	// (DIGEST_BRIEF_HOUR_UTC). There is no per-user time zone.
	BriefHourUTC int
	// Learning turns on the weekly learning job: one per tenant per ISO week
	// (G16), for tenants with a workspace that allows the learning agent. It
	// needs Jobs and Workspaces.
	Learning   bool
	Workspaces repository.WorkspaceRepository
}

// Scheduler writes task.due_soon and task.overdue events, queues
// blocker_investigator jobs for stale tasks (section 3.3), and queues the
// day's morning briefs and the week's learning jobs.
//
// Replicas may run it at the same time: each batch claims its tasks with
// SKIP LOCKED and marks them in the same transaction, and the unique
// idempotency key stops a second row should two ever overlap. A stale task
// is marked by its job, whose dedup key names the day, so it is
// investigated at most once per day however many scans see it. A brief is
// marked the same way, by its job's key naming the person and the day.
type Scheduler struct {
	cfg    SchedulerConfig
	tx     repository.TxManager
	tasks  repository.TaskRepository
	outbox repository.OutboxRepository

	mu sync.Mutex
	// briefed is the last day whose brief scan ran to the end in this
	// process; later ticks that day skip the scan.
	briefed string
	// learned is the last ISO week whose learning scan ran to the end in
	// this process.
	learned string
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
	if s.cfg.Jobs != nil && s.cfg.Briefs {
		n, err := s.briefs(ctx, now)
		if err != nil {
			return total, err
		}
		if n > 0 {
			s.cfg.Logger.Info("deadline scheduler: queued morning briefs", "briefs", n)
		}
	}
	if s.cfg.Jobs != nil && s.cfg.Workspaces != nil && s.cfg.Learning {
		n, err := s.learning(ctx, now)
		if err != nil {
			return total, err
		}
		if n > 0 {
			s.cfg.Logger.Info("deadline scheduler: queued learning jobs", "tenants", n)
		}
	}
	return total, nil
}

// learning queues the week's learning jobs and returns how many it queued:
// one per tenant per ISO week, from the first tick of the week (G16). As for
// briefs, the first scan of the week that runs to the end is remembered, a
// scan by another replica or after a restart finds the jobs queued and adds
// none, settings are checked by the query, and the jobs are queued whatever
// AGENTS_ENABLED is.
func (s *Scheduler) learning(ctx context.Context, now time.Time) (int, error) {
	week := domain.LearningWeek(now)
	s.mu.Lock()
	done := s.learned == week
	s.mu.Unlock()
	if done {
		return 0, nil
	}
	queued := 0
	for {
		found, created := 0, 0
		err := s.tx.WithTx(ctx, func(ctx context.Context) error {
			found, created = 0, 0 // a retried transaction starts again
			tenants, err := s.cfg.Workspaces.ListLearningTenants(ctx, week, s.cfg.BatchSize)
			if err != nil {
				return err
			}
			found = len(tenants)
			for _, lt := range tenants {
				job, err := domain.NewLearningJob(lt, now)
				if err != nil {
					return err
				}
				ok, err := s.cfg.Jobs.Insert(ctx, job)
				if err != nil {
					return err
				}
				if ok {
					created++
				}
			}
			return nil
		})
		queued += created
		if err != nil {
			return queued, err
		}
		if found < s.cfg.BatchSize {
			break
		}
		if created == 0 {
			// Another replica's scan holds this batch; look again next tick.
			return queued, nil
		}
	}
	s.mu.Lock()
	s.learned = week
	s.mu.Unlock()
	return queued, nil
}

// briefs queues the day's morning briefs once the brief hour has passed
// (G15) and returns how many it queued. The first scan of the day that runs
// to the end is remembered, so later ticks cost nothing; a scan by another
// replica, or after a restart, finds the jobs already queued and adds none.
// Settings are checked by the query, as for the stale scan, and the jobs are
// queued whatever AGENTS_ENABLED is, as a task change's jobs are.
func (s *Scheduler) briefs(ctx context.Context, now time.Time) (int, error) {
	at := time.Date(now.Year(), now.Month(), now.Day(), s.cfg.BriefHourUTC, 0, 0, 0, time.UTC)
	if now.Before(at) {
		return 0, nil
	}
	day := at.Format(time.DateOnly)
	s.mu.Lock()
	done := s.briefed == day
	s.mu.Unlock()
	if done {
		return 0, nil
	}
	queued := 0
	for {
		found, created, err := s.briefBatch(ctx, at, now)
		queued += created
		if err != nil {
			return queued, err
		}
		if found < s.cfg.BatchSize {
			break
		}
		if created == 0 {
			// Another replica's scan holds this batch; look again next
			// tick rather than spin on it.
			return queued, nil
		}
	}
	s.mu.Lock()
	s.briefed = day
	s.mu.Unlock()
	return queued, nil
}

// briefBatch queues the briefs of up to BatchSize people who have none for
// the day yet. It returns how many people it found and how many briefs it
// queued; the unique dedup key stops a brief another scan queued first.
func (s *Scheduler) briefBatch(ctx context.Context, at, now time.Time) (found, created int, err error) {
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		found, created = 0, 0 // a retried transaction starts again
		people, err := s.tasks.ListBriefRecipients(ctx, at, s.cfg.BatchSize)
		if err != nil {
			return err
		}
		found = len(people)
		for _, p := range people {
			job, err := domain.NewBriefJob(p, at, now)
			if err != nil {
				return err
			}
			ok, err := s.cfg.Jobs.Insert(ctx, job)
			if err != nil {
				return err
			}
			if ok {
				created++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return found, created, nil
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
		entries, err := domain.NewOutboxEntries(events, domain.Links{API: s.cfg.PublicBaseURL, App: s.cfg.AppBaseURL}, s.cfg.OutboxMaxAttempts, now)
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
