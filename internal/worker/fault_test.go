package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

var errInjected = errors.New("injected failure")

// faults fails the repository calls a test names; every other call passes
// through. Calls are named "Repo.Method", such as "Outbox.Insert".
type faults struct {
	mu    sync.Mutex
	plans map[string]*faultPlan
	calls map[string]int
}

type faultPlan struct{ skip, fail int }

func newFaults() *faults {
	return &faults{plans: map[string]*faultPlan{}, calls: map[string]int{}}
}

// fail lets the next skip calls of method through and then fails n calls;
// n < 0 fails every later call.
func (f *faults) fail(method string, skip, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans[method] = &faultPlan{skip: skip, fail: n}
}

func (f *faults) hit(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	p := f.plans[method]
	switch {
	case p == nil || p.fail == 0:
		return nil
	case p.skip > 0:
		p.skip--
		return nil
	case p.fail > 0:
		p.fail--
	}
	return fmt.Errorf("%s: %w", method, errInjected)
}

// count returns how many times method was called.
func (f *faults) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

type faultTasks struct {
	repository.TaskRepository
	f *faults
}

func (r faultTasks) ClaimDueSoon(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error) {
	if err := r.f.hit("Tasks.ClaimDueSoon"); err != nil {
		return nil, err
	}
	return r.TaskRepository.ClaimDueSoon(ctx, now, limit)
}
func (r faultTasks) ClaimOverdue(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error) {
	if err := r.f.hit("Tasks.ClaimOverdue"); err != nil {
		return nil, err
	}
	return r.TaskRepository.ClaimOverdue(ctx, now, limit)
}
func (r faultTasks) ClaimStale(ctx context.Context, staleBefore, now time.Time, limit int) ([]*domain.Task, error) {
	if err := r.f.hit("Tasks.ClaimStale"); err != nil {
		return nil, err
	}
	return r.TaskRepository.ClaimStale(ctx, staleBefore, now, limit)
}
func (r faultTasks) ListBriefRecipients(ctx context.Context, day time.Time, limit int) ([]domain.BriefRecipient, error) {
	if err := r.f.hit("Tasks.ListBriefRecipients"); err != nil {
		return nil, err
	}
	return r.TaskRepository.ListBriefRecipients(ctx, day, limit)
}
func (r faultTasks) MarkDueSoonNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	if err := r.f.hit("Tasks.MarkDueSoonNotified"); err != nil {
		return err
	}
	return r.TaskRepository.MarkDueSoonNotified(ctx, tenantID, id, at)
}
func (r faultTasks) MarkOverdueNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	if err := r.f.hit("Tasks.MarkOverdueNotified"); err != nil {
		return err
	}
	return r.TaskRepository.MarkOverdueNotified(ctx, tenantID, id, at)
}

type faultJobs struct {
	repository.AgentJobRepository
	f *faults
}

func (r faultJobs) Insert(ctx context.Context, j domain.AgentJob) (bool, error) {
	if err := r.f.hit("Jobs.Insert"); err != nil {
		return false, err
	}
	return r.AgentJobRepository.Insert(ctx, j)
}

type faultOutbox struct {
	repository.OutboxRepository
	f *faults
}

func (r faultOutbox) Insert(ctx context.Context, entries []domain.OutboxEntry) (int, error) {
	if err := r.f.hit("Outbox.Insert"); err != nil {
		return 0, err
	}
	return r.OutboxRepository.Insert(ctx, entries)
}
func (r faultOutbox) ClaimBatch(ctx context.Context, now time.Time, limit int) ([]domain.OutboxEntry, error) {
	if err := r.f.hit("Outbox.ClaimBatch"); err != nil {
		return nil, err
	}
	return r.OutboxRepository.ClaimBatch(ctx, now, limit)
}
func (r faultOutbox) MarkDispatched(ctx context.Context, id uuid.UUID, claimedAt, at time.Time) error {
	if err := r.f.hit("Outbox.MarkDispatched"); err != nil {
		return err
	}
	return r.OutboxRepository.MarkDispatched(ctx, id, claimedAt, at)
}
func (r faultOutbox) ReleaseExpiredLeases(ctx context.Context, olderThan time.Time) (int, error) {
	if err := r.f.hit("Outbox.ReleaseExpiredLeases"); err != nil {
		return 0, err
	}
	return r.OutboxRepository.ReleaseExpiredLeases(ctx, olderThan)
}
func (r faultOutbox) DeleteDispatchedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if err := r.f.hit("Outbox.DeleteDispatchedBefore"); err != nil {
		return 0, err
	}
	return r.OutboxRepository.DeleteDispatchedBefore(ctx, cutoff, limit)
}

type faultDeliveries struct {
	repository.WebhookDeliveryRepository
	f *faults
}

func (r faultDeliveries) DeleteBefore(ctx context.Context, cutoff time.Time) (int, error) {
	if err := r.f.hit("Deliveries.DeleteBefore"); err != nil {
		return 0, err
	}
	return r.WebhookDeliveryRepository.DeleteBefore(ctx, cutoff)
}

// logBuffer collects a worker's log lines; workers log from their own
// goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) logger() *slog.Logger { return slog.New(slog.NewTextHandler(l, nil)) }

func (l *logBuffer) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), s)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// enableAgents turns agents on in the scheduler fixture's workspace.
func (f *schedFixture) enableAgents(t *testing.T) {
	t.Helper()
	w, err := f.b.Workspaces.Get(ctx, tenant, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Workspaces.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerDefaults(t *testing.T) {
	b := repotest.Memory()
	s := NewScheduler(b.Tx, b.Tasks, b.Outbox, SchedulerConfig{})
	if c := s.cfg; c.Interval != time.Minute || c.BatchSize != 100 || c.OutboxMaxAttempts != 10 ||
		c.StaleAfter != domain.StaleAfter || c.Logger != slog.Default() || c.Now == nil || c.Wake == nil {
		t.Errorf("scheduler defaults = %+v", c)
	}
	s.cfg.Wake() // the default wakes nobody

	c := NewCleanup(b.Outbox, CleanupConfig{})
	if cfg := c.cfg; cfg.Interval != 24*time.Hour || cfg.Retention != 7*24*time.Hour || cfg.BatchSize != 5000 ||
		cfg.DeliveryRetention != time.Hour || cfg.Logger != slog.Default() || cfg.Now == nil {
		t.Errorf("cleanup defaults = %+v", cfg)
	}

	r := NewRelay(b.Outbox, nil, RelayConfig{})
	if cfg := r.cfg; cfg.Workers != 8 || cfg.BatchSize != 100 || cfg.PollInterval != 200*time.Millisecond ||
		cfg.Lease != time.Minute || cfg.OpenCircuitDelay != 10*time.Second || cfg.Backoff == nil ||
		cfg.Logger != slog.Default() || cfg.Now == nil {
		t.Errorf("relay defaults = %+v", cfg)
	}
}

// TestSchedulerFailedBatchWritesNothing: a reminder batch claims, marks,
// and writes its rows in one transaction, so a failure at any step leaves
// the batch's tasks unmarked and unreminded, and the next tick reminds
// them exactly once.
func TestSchedulerFailedBatchWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		step string
		// soonSent is whether the due-soon batch, which runs first, had
		// already committed when step failed.
		soonSent bool
	}{
		{"Tasks.ClaimDueSoon", false},
		{"Tasks.MarkDueSoonNotified", false},
		{"Outbox.Insert", false}, // the due-soon batch's rows
		{"Tasks.ClaimOverdue", true},
		{"Tasks.MarkOverdueNotified", true},
	} {
		t.Run(tc.step, func(t *testing.T) {
			each(t, func(t *testing.T, b repotest.Backend) {
				f := newSchedFixture(t, b)
				soon := f.task(t, t0.Add(30*time.Minute), "usr_bob")
				late := f.task(t, t0.Add(-time.Hour), "usr_bob")
				fs := newFaults()
				fs.fail(tc.step, 0, 1)
				woken := 0
				s := NewScheduler(b.Tx, faultTasks{b.Tasks, fs}, faultOutbox{b.Outbox, fs}, SchedulerConfig{
					PublicBaseURL: "https://taskify.test", Logger: quiet,
					Now: func() time.Time { return t0 }, Wake: func() { woken++ },
				})
				reminded := func() (int, int) {
					return len(f.reminders(t, soon.ID, domain.EventTaskDueSoon)), len(f.reminders(t, late.ID, domain.EventTaskOverdue))
				}

				if _, err := s.Tick(ctx); !errors.Is(err, errInjected) {
					t.Fatalf("tick = %v, want the injected failure", err)
				}
				wantSoon := 0
				if tc.soonSent {
					wantSoon = 1
				}
				if gotSoon, gotLate := reminded(); gotSoon != wantSoon || gotLate != 0 {
					t.Fatalf("after the failed tick: due-soon %d, overdue %d; want %d and 0", gotSoon, gotLate, wantSoon)
				}
				if woken != 0 {
					t.Errorf("a failed tick woke the relay")
				}

				// The failed batch's task was not marked: the next tick
				// reminds it, and nothing is reminded twice.
				n, err := s.Tick(ctx)
				if err != nil || n != 2-wantSoon {
					t.Fatalf("retry tick = %d, %v; want %d", n, err, 2-wantSoon)
				}
				if gotSoon, gotLate := reminded(); gotSoon != 1 || gotLate != 1 {
					t.Errorf("after the retry: due-soon %d, overdue %d; want one each", gotSoon, gotLate)
				}
				if n, err := s.Tick(ctx); err != nil || n != 0 {
					t.Errorf("third tick = %d, %v; want nothing new", n, err)
				}
				if woken != 1 {
					t.Errorf("relay woken %d times, want once, by the retry", woken)
				}
			})
		})
	}
}

// TestSchedulerFailedStaleBatchIsRetried: a stale task is marked only by
// its job, so a scan that fails before the job is stored leaves the task
// to the next scan.
func TestSchedulerFailedStaleBatchIsRetried(t *testing.T) {
	for _, step := range []string{"Tasks.ClaimStale", "Jobs.Insert"} {
		t.Run(step, func(t *testing.T) {
			each(t, func(t *testing.T, b repotest.Backend) {
				f := newSchedFixture(t, b)
				f.enableAgents(t)
				task := f.task(t, t0.Add(30*24*time.Hour), "usr_bob")
				status := domain.StatusInProgress
				if _, err := f.svc.UpdateTask(ctx, domain.User("usr_alice"), tenant, task.ID, task.Version, domain.Patch{Status: &status}); err != nil {
					t.Fatal(err)
				}
				fs := newFaults()
				fs.fail(step, 0, 1)
				now := t0.Add(72 * time.Hour)
				s := NewScheduler(b.Tx, faultTasks{b.Tasks, fs}, b.Outbox, SchedulerConfig{
					Logger: quiet, Jobs: faultJobs{b.AgentJobs, fs}, Now: func() time.Time { return now },
				})

				if _, err := s.Tick(ctx); !errors.Is(err, errInjected) {
					t.Fatalf("tick = %v, want the injected failure", err)
				}
				if keys := blockerJobs(t, b, task.ID); len(keys) != 0 {
					t.Fatalf("the failed scan queued %v", keys)
				}
				if _, err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if keys := blockerJobs(t, b, task.ID); len(keys) != 1 || keys[0] != domain.StaleJobDedupKey(domain.AgentBlocker, task.ID, now) {
					t.Errorf("after the retry: %v, want today's investigation", keys)
				}
			})
		})
	}
}

// briefFixture is a scheduler fixture with agents on and one open task for
// usr_bob, so the brief scan has one person to brief.
func briefFixture(t *testing.T, b repotest.Backend) *schedFixture {
	t.Helper()
	f := newSchedFixture(t, b)
	f.enableAgents(t)
	f.task(t, t0.Add(30*24*time.Hour), "usr_bob")
	return f
}

// TestSchedulerFailedBriefScanIsRetried: the day is remembered as briefed
// only once a scan runs to the end, so a failed scan is retried on the
// next tick the same day.
func TestSchedulerFailedBriefScanIsRetried(t *testing.T) {
	for _, step := range []string{"Tasks.ListBriefRecipients", "Jobs.Insert"} {
		t.Run(step, func(t *testing.T) {
			each(t, func(t *testing.T, b repotest.Backend) {
				briefFixture(t, b)
				day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
				now := day.Add(8*time.Hour + 30*time.Minute)
				fs := newFaults()
				fs.fail(step, 0, 1)
				s := NewScheduler(b.Tx, faultTasks{b.Tasks, fs}, b.Outbox, SchedulerConfig{
					Logger: quiet, Jobs: faultJobs{b.AgentJobs, fs}, Now: func() time.Time { return now },
					Briefs: true, BriefHourUTC: 8,
				})

				if _, err := s.Tick(ctx); !errors.Is(err, errInjected) {
					t.Fatalf("tick = %v, want the injected failure", err)
				}
				if got := queuedBriefs(t, b, now); len(got) != 0 {
					t.Fatalf("the failed scan queued %v", got)
				}
				if _, err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if got := queuedBriefs(t, b, now); len(got) != 1 || got[0] != domain.BriefJobDedupKey(tenant, "usr_bob", day) {
					t.Fatalf("after the retry: %v, want bob's brief", got)
				}
				// The scan ran to the end: later ticks that day skip it.
				scans := fs.count("Tasks.ListBriefRecipients")
				if _, err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if got := fs.count("Tasks.ListBriefRecipients"); got != scans {
					t.Errorf("a later tick scanned again: %d scans, want %d", got, scans)
				}
			})
		})
	}
}

// heldRecipients answers ListBriefRecipients from a list read earlier, as
// a replica's query does while another replica's scan has queued those
// briefs but not yet committed.
type heldRecipients struct {
	repository.TaskRepository
	held  []domain.BriefRecipient
	calls int
}

func (h *heldRecipients) ListBriefRecipients(ctx context.Context, day time.Time, limit int) ([]domain.BriefRecipient, error) {
	h.calls++
	if h.held != nil {
		return h.held, nil
	}
	return h.TaskRepository.ListBriefRecipients(ctx, day, limit)
}

// TestBriefScanYieldsToAnotherReplica: a full batch of people whose briefs
// another replica queued first adds nothing; the scan stops rather than
// spin on that batch, and looks again on the next tick.
func TestBriefScanYieldsToAnotherReplica(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		briefFixture(t, b)
		day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
		now := day.Add(9 * time.Hour)
		cfg := SchedulerConfig{BatchSize: 1, Logger: quiet, Jobs: b.AgentJobs, Now: func() time.Time { return now }, Briefs: true, BriefHourUTC: 8}

		held, err := b.Tasks.ListBriefRecipients(ctx, day.Add(8*time.Hour), 1)
		if err != nil || len(held) != 1 {
			t.Fatalf("recipients = %v, %v", held, err)
		}
		if _, err := NewScheduler(b.Tx, b.Tasks, b.Outbox, cfg).Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := queuedBriefs(t, b, now); len(got) != 1 {
			t.Fatalf("first replica queued %v", got)
		}

		tasks := &heldRecipients{TaskRepository: b.Tasks, held: held}
		late := NewScheduler(b.Tx, tasks, b.Outbox, cfg)
		if _, err := late.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if tasks.calls != 1 {
			t.Errorf("the scan read %d batches, want it to stop after the first", tasks.calls)
		}
		if got := queuedBriefs(t, b, now); len(got) != 0 {
			t.Errorf("second replica queued %v", got)
		}

		// The day was not remembered: the next tick scans again, finds
		// nobody left, and only then skips the rest of the day.
		tasks.held = nil
		for i := 0; i < 2; i++ {
			if _, err := late.Tick(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if tasks.calls != 2 {
			t.Errorf("scans = %d, want one more, then none", tasks.calls)
		}
		if got := queuedBriefs(t, b, now); len(got) != 0 {
			t.Errorf("the rescan queued %v", got)
		}
	})
}

// TestSchedulerRunSurvivesAFailedTick: Run logs a failed tick and keeps
// ticking, and the next tick does the work.
func TestSchedulerRunSurvivesAFailedTick(t *testing.T) {
	b := repotest.Memory()
	f := newSchedFixture(t, b)
	late := f.task(t, t0.Add(-time.Hour), "usr_bob")
	fs := newFaults()
	fs.fail("Tasks.ClaimOverdue", 0, 1)
	logs := &logBuffer{}
	s := NewScheduler(b.Tx, faultTasks{b.Tasks, fs}, b.Outbox, SchedulerConfig{
		Interval: 5 * time.Millisecond, Logger: logs.logger(), Now: func() time.Time { return t0 },
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	waitFor(t, "a later tick to remind", func() bool { return len(f.reminders(t, late.ID, domain.EventTaskOverdue)) == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}
	if !logs.has("deadline scheduler: tick failed") || !logs.has(errInjected.Error()) {
		t.Errorf("the failed tick was not logged:\n%s", logs)
	}
}

// TestCleanupFailures: a failure stops the run and reports what was
// already deleted; nothing is lost, and the next run finishes the job.
func TestCleanupFailures(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		now := t0.Add(30 * 24 * time.Hour)
		seedOutbox(t, b, 3, 10, t0)
		claimed, _ := b.Outbox.ClaimBatch(ctx, t0, 10)
		for _, e := range claimed {
			if err := b.Outbox.MarkDispatched(ctx, e.ID, *e.ClaimedAt, now.Add(-8*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := b.Deliveries.Record(ctx, "old-sig", now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		fs := newFaults()
		c := NewCleanup(faultOutbox{b.Outbox, fs}, CleanupConfig{
			BatchSize: 2, Deliveries: faultDeliveries{b.Deliveries, fs}, Now: func() time.Time { return now }, Logger: quiet,
		})

		// The delivery records go first; when they fail, no row is touched.
		fs.fail("Deliveries.DeleteBefore", 0, 1)
		if n, err := c.RunOnce(ctx); !errors.Is(err, errInjected) || n != 0 {
			t.Fatalf("RunOnce = %d, %v; want 0 and the injected failure", n, err)
		}
		if got := count(t, b, domain.OutboxDispatched); got != 3 {
			t.Fatalf("dispatched rows = %d, want all 3 kept", got)
		}

		// The second outbox batch fails: the delivery and the first batch
		// are reported deleted.
		fs.fail("Outbox.DeleteDispatchedBefore", 1, 1)
		if n, err := c.RunOnce(ctx); !errors.Is(err, errInjected) || n != 3 {
			t.Fatalf("RunOnce = %d, %v; want 3 (1 delivery, 2 rows) and the injected failure", n, err)
		}
		if got := count(t, b, domain.OutboxDispatched); got != 1 {
			t.Fatalf("dispatched rows = %d, want 1 left", got)
		}
		if fresh, _ := b.Deliveries.Record(ctx, "old-sig", now); !fresh {
			t.Error("the expired delivery was kept")
		}

		if n, err := c.RunOnce(ctx); err != nil || n != 1 {
			t.Errorf("RunOnce = %d, %v; want the last row", n, err)
		}
		if got := count(t, b, domain.OutboxDispatched); got != 0 {
			t.Errorf("dispatched rows = %d, want none", got)
		}
	})
}

// TestCleanupRunLogs: Run logs a failed run and keeps going, and logs how
// many rows the next run deleted.
func TestCleanupRunLogs(t *testing.T) {
	b := repotest.Memory()
	now := t0.Add(30 * 24 * time.Hour)
	seedOutbox(t, b, 2, 10, t0)
	claimed, _ := b.Outbox.ClaimBatch(ctx, t0, 10)
	for _, e := range claimed {
		if err := b.Outbox.MarkDispatched(ctx, e.ID, *e.ClaimedAt, now.Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	fs := newFaults()
	fs.fail("Outbox.DeleteDispatchedBefore", 0, 1)
	logs := &logBuffer{}
	c := NewCleanup(faultOutbox{b.Outbox, fs}, CleanupConfig{Interval: 5 * time.Millisecond, Now: func() time.Time { return now }, Logger: logs.logger()})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	waitFor(t, "expired rows deleted", func() bool { return count(t, b, domain.OutboxDispatched) == 0 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}
	if !logs.has("outbox cleanup failed") || !logs.has("deleted=2") {
		t.Errorf("logs:\n%s", logs)
	}
}

// TestRelaySurvivesRepositoryFailures: a failed claim is logged and the
// next poll claims; a send whose result cannot be recorded stays claimed
// until its lease expires, and the reaper, after a failed pass of its own,
// returns it to be sent again, which AgentPulse deduplicates by key.
func TestRelaySurvivesRepositoryFailures(t *testing.T) {
	b := repotest.Memory()
	mock, client := newMock(t)
	seedOutbox(t, b, 1, 10, time.Now().Add(-time.Second))
	fs := newFaults()
	fs.fail("Outbox.ClaimBatch", 0, 1)
	fs.fail("Outbox.MarkDispatched", 0, 1)
	fs.fail("Outbox.ReleaseExpiredLeases", 0, 1)
	logs := &logBuffer{}
	r := NewRelay(faultOutbox{b.Outbox, fs}, client, RelayConfig{
		PollInterval: 5 * time.Millisecond, Lease: 50 * time.Millisecond, Logger: logs.logger(),
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	waitFor(t, "the row dispatched", func() bool { return count(t, b, domain.OutboxDispatched) == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}

	if len(mock.Notifications()) != 1 || mock.Duplicates() != 1 {
		t.Errorf("AgentPulse accepted %d and deduplicated %d; want the resend deduplicated",
			len(mock.Notifications()), mock.Duplicates())
	}
	for _, msg := range []string{
		"outbox relay: claim failed", "outbox relay: could not record result", "outbox relay: lease reaper failed",
		"outbox relay: returned abandoned rows to pending",
	} {
		if !logs.has(msg) {
			t.Errorf("not logged: %q\n%s", msg, logs)
		}
	}
}
