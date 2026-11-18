package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

var (
	ctx    = context.Background()
	t0     = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	tenant = "tenant_acme"
	quiet  = slog.New(slog.NewTextHandler(io.Discard, nil))
)

func each(t *testing.T, fn func(t *testing.T, b repotest.Backend)) {
	t.Helper()
	for _, f := range repotest.Factories(t) {
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			fn(t, f.New(t))
		})
	}
}

func newMock(t *testing.T) (*mockagentpulse.Server, *agentpulse.Client) {
	t.Helper()
	m := mockagentpulse.New()
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return m, agentpulse.New(ts.URL, "test-key", agentpulse.WithBreaker(nil))
}

// seedOutbox inserts n pending rows, each a valid /notify body with a
// distinct idempotency key.
func seedOutbox(t *testing.T, b repotest.Backend, n, maxAttempts int, at time.Time) []domain.OutboxEntry {
	t.Helper()
	var entries []domain.OutboxEntry
	agg := uuid.New()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("taskify:%s:%s:task.updated:v%d", tenant, agg, i)
		body, _ := json.Marshal(domain.Notification{
			IdempotencyKey: key, TenantID: tenant, UserID: "usr_bob", Source: "taskify",
			EventType: domain.EventTaskUpdated, Title: "t", Body: "b", PriorityHint: "medium",
			Payload: map[string]any{"n": i},
		})
		entries = append(entries, domain.OutboxEntry{
			ID: uuid.New(), TenantID: tenant, AggregateID: agg, EventType: domain.EventTaskUpdated,
			IdempotencyKey: key, Payload: body, MaxAttempts: maxAttempts, NextAttemptAt: at, CreatedAt: at,
		})
	}
	if _, err := b.Outbox.Insert(ctx, entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func count(t *testing.T, b repotest.Backend, s domain.OutboxStatus) int {
	t.Helper()
	n, err := b.Outbox.CountByStatus(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRelayDeliversEachKeyExactlyOnce: two relays (as two replicas) with four
// workers each drain 1,000 rows, and AgentPulse receives every key once.
func TestRelayDeliversEachKeyExactlyOnce(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		const rows = 1000
		mock, client := newMock(t)
		seedOutbox(t, b, rows, 10, time.Now().Add(-time.Second))

		runCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			r := NewRelay(b.Outbox, client, RelayConfig{Workers: 4, BatchSize: 50, PollInterval: 5 * time.Millisecond, Logger: quiet})
			wg.Add(1)
			go func() { defer wg.Done(); _ = r.Run(runCtx) }()
		}
		waitFor(t, "all rows dispatched", func() bool { return count(t, b, domain.OutboxDispatched) == rows })
		cancel()
		wg.Wait()

		if got := len(mock.RequestsTo("POST", "/notify")); got != rows {
			t.Errorf("/notify requests = %d, want %d", got, rows)
		}
		if mock.Duplicates() != 0 {
			t.Errorf("AgentPulse saw %d duplicate keys", mock.Duplicates())
		}
		if len(mock.Notifications()) != rows {
			t.Errorf("accepted notifications = %d", len(mock.Notifications()))
		}
	})
}

func TestRelayRetriesThenSucceeds(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		row := seedOutbox(t, b, 1, 10, t0)[0]
		mock.FailNext("POST", "/notify", http.StatusServiceUnavailable, 2)

		now := t0
		var delays []int
		r := NewRelay(b.Outbox, client, RelayConfig{
			Now: func() time.Time { return now }, Logger: quiet,
			Backoff: func(attempts int) time.Duration { delays = append(delays, attempts); return time.Minute },
		})
		for i := 0; i < 3; i++ {
			if n, err := r.RunOnce(ctx); err != nil || n != 1 {
				t.Fatalf("round %d: claimed %d, %v", i, n, err)
			}
			if i < 2 {
				// Not due again until the backoff has passed.
				if n, _ := r.RunOnce(ctx); n != 0 {
					t.Fatalf("row claimed again before its backoff ended")
				}
				now = now.Add(time.Minute)
			}
		}
		got, _ := b.Outbox.Get(ctx, row.ID)
		if got.Status != domain.OutboxDispatched || got.Attempts != 2 || got.LastError == "" {
			t.Errorf("row = %s attempts %d error %q", got.Status, got.Attempts, got.LastError)
		}
		if len(delays) != 2 || delays[0] != 1 || delays[1] != 2 {
			t.Errorf("backoff called with %v, want [1 2]", delays)
		}
		if len(mock.Notifications()) != 1 {
			t.Errorf("accepted = %d, want 1", len(mock.Notifications()))
		}
	})
}

func TestRelayDeadLettersAfterMaxAttempts(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		row := seedOutbox(t, b, 1, 10, t0)[0]
		mock.FailNext("POST", "/notify", http.StatusServiceUnavailable, -1)

		r := NewRelay(b.Outbox, client, RelayConfig{Now: func() time.Time { return t0 }, Backoff: func(int) time.Duration { return 0 }, Logger: quiet})
		for i := 0; i < 15; i++ {
			if _, err := r.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := b.Outbox.Get(ctx, row.ID)
		if got.Status != domain.OutboxDeadLetter || got.Attempts != 10 {
			t.Errorf("row = %s after %d attempts, want dead_letter after 10", got.Status, got.Attempts)
		}
		if n := len(mock.RequestsTo("POST", "/notify")); n != 10 {
			t.Errorf("/notify requests = %d, want 10", n)
		}
	})
}

func TestRelayDeadLettersPermanentRejection(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		row := seedOutbox(t, b, 1, 10, t0)[0]
		mock.FailNext("POST", "/notify", http.StatusUnprocessableEntity, 1)

		r := NewRelay(b.Outbox, client, RelayConfig{Now: func() time.Time { return t0 }, Logger: quiet})
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := b.Outbox.Get(ctx, row.ID)
		if got.Status != domain.OutboxDeadLetter || got.Attempts != 1 {
			t.Errorf("row = %s attempts %d, want dead_letter after 1", got.Status, got.Attempts)
		}
	})
}

func TestRelayOpenCircuitIsNotAnAttempt(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock := mockagentpulse.New()
		ts := httptest.NewServer(mock)
		defer ts.Close()
		breaker := agentpulse.NewBreaker(2, 0.5, time.Hour)
		client := agentpulse.New(ts.URL, "k", agentpulse.WithBreaker(breaker))
		for i := 0; i < 2; i++ { // trip it
			breaker.Allow()
			breaker.Record(true)
		}

		row := seedOutbox(t, b, 1, 10, t0)[0]
		r := NewRelay(b.Outbox, client, RelayConfig{Now: func() time.Time { return t0 }, OpenCircuitDelay: 30 * time.Second, Logger: quiet})
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := b.Outbox.Get(ctx, row.ID)
		if got.Status != domain.OutboxPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(t0.Add(30*time.Second)) {
			t.Errorf("row = %s attempts %d next %v", got.Status, got.Attempts, got.NextAttemptAt)
		}
		if len(mock.Requests()) != 0 {
			t.Error("AgentPulse was contacted while the circuit was open")
		}
	})
}

// TestRelayRecoversAbandonedRows: a worker that crashed after claiming leaves
// rows in processing; the lease reaper returns them and they are delivered.
func TestRelayRecoversAbandonedRows(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		seedOutbox(t, b, 3, 10, time.Now().Add(-time.Minute))
		if claimed, _ := b.Outbox.ClaimBatch(ctx, time.Now().Add(-time.Second), 10); len(claimed) != 3 {
			t.Fatalf("crashed worker claimed %d rows", len(claimed))
		}

		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		r := NewRelay(b.Outbox, client, RelayConfig{Lease: 100 * time.Millisecond, PollInterval: 10 * time.Millisecond, Logger: quiet})
		done := make(chan struct{})
		go func() { _ = r.Run(runCtx); close(done) }()

		waitFor(t, "abandoned rows delivered", func() bool { return count(t, b, domain.OutboxDispatched) == 3 })
		cancel()
		<-done
		if n := len(mock.Notifications()); n != 3 {
			t.Errorf("accepted = %d, want 3", n)
		}
	})
}

func TestRelayWakesImmediately(t *testing.T) {
	b := repotest.Memory()
	mock, client := newMock(t)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := NewRelay(b.Outbox, client, RelayConfig{PollInterval: time.Hour, Logger: quiet})
	go func() { _ = r.Run(runCtx) }()

	seedOutbox(t, b, 1, 10, time.Now().Add(-time.Second))
	r.Wake()
	r.Wake() // a second signal while one is pending must not block
	waitFor(t, "woken relay to send", func() bool { return len(mock.Notifications()) == 1 })
}

func TestFullJitter(t *testing.T) {
	for attempts, ceiling := range map[int]time.Duration{
		0: 500 * time.Millisecond, 1: time.Second, 3: 4 * time.Second, 6: 32 * time.Second,
		7: time.Minute, 10: time.Minute, 64: time.Minute,
	} {
		for i := 0; i < 200; i++ {
			if d := FullJitter(attempts); d < 0 || d > ceiling {
				t.Fatalf("FullJitter(%d) = %v, want within [0, %v]", attempts, d, ceiling)
			}
		}
	}
}

// --- scheduler -------------------------------------------------------------

type schedFixture struct {
	b   repotest.Backend
	svc *service.TaskService
}

func newSchedFixture(t *testing.T, b repotest.Backend) *schedFixture {
	t.Helper()
	svc := service.NewTaskService(
		service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments, Outbox: b.Outbox},
		service.Config{PublicBaseURL: "https://taskify.test"},
		service.WithClock(func() time.Time { return t0 }),
	)
	if _, err := svc.CreateWorkspace(ctx, tenant, "ws", "W"); err != nil {
		t.Fatal(err)
	}
	return &schedFixture{b: b, svc: svc}
}

func (f *schedFixture) task(t *testing.T, due time.Time, assignee string) *domain.Task {
	t.Helper()
	task, err := f.svc.CreateTask(ctx, domain.User("usr_alice"), domain.NewTaskInput{
		TenantID: tenant, WorkspaceID: "ws", Title: "Ship it", AssigneeID: assignee, DueAt: &due,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *schedFixture) scheduler(now *time.Time) *Scheduler {
	return NewScheduler(f.b.Tx, f.b.Tasks, f.b.Outbox, SchedulerConfig{
		BatchSize: 7, PublicBaseURL: "https://taskify.test", Logger: quiet,
		Now: func() time.Time { return *now },
	})
}

// reminders returns the idempotency keys of reminder rows for a task.
func (f *schedFixture) reminders(t *testing.T, taskID uuid.UUID, eventType string) []string {
	t.Helper()
	entries, err := f.b.Outbox.ListForAggregate(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range entries {
		if e.EventType == eventType {
			keys = append(keys, e.IdempotencyKey)
		}
	}
	return keys
}

// TestTwoSchedulersOneOverduePerDay is the M2 exit criterion.
func TestTwoSchedulersOneOverduePerDay(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newSchedFixture(t, b)
		const tasks = 30
		var ids []uuid.UUID
		for i := 0; i < tasks; i++ {
			ids = append(ids, f.task(t, t0.Add(-time.Hour), "usr_bob").ID)
		}
		now := t0
		a, c := f.scheduler(&now), f.scheduler(&now)

		var wg sync.WaitGroup
		for _, s := range []*Scheduler{a, c, a, c} {
			wg.Add(1)
			go func(s *Scheduler) {
				defer wg.Done()
				if _, err := s.Tick(ctx); err != nil {
					t.Error(err)
				}
			}(s)
		}
		wg.Wait()
		for _, id := range ids {
			if n := len(f.reminders(t, id, domain.EventTaskOverdue)); n != 1 {
				t.Fatalf("task has %d overdue reminders, want 1", n)
			}
		}

		// Later the same day: nothing new. The next day: one more each.
		now = t0.Add(10 * time.Hour)
		if n, _ := a.Tick(ctx); n != 0 {
			t.Errorf("same-day tick wrote %d reminders", n)
		}
		now = t0.Add(24 * time.Hour)
		if n, _ := c.Tick(ctx); n != tasks {
			t.Errorf("next-day tick wrote %d reminders, want %d", n, tasks)
		}
		if keys := f.reminders(t, ids[0], domain.EventTaskOverdue); len(keys) != 2 || keys[0] == keys[1] {
			t.Errorf("overdue keys = %v", keys)
		}
	})
}

func TestDueSoonWindowsAndReschedule(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newSchedFixture(t, b)
		due := t0.Add(30 * time.Hour)
		task := f.task(t, due, "usr_bob")
		woken := 0
		now := t0
		s := NewScheduler(b.Tx, b.Tasks, b.Outbox, SchedulerConfig{
			PublicBaseURL: "https://taskify.test", Logger: quiet,
			Now: func() time.Time { return now }, Wake: func() { woken++ },
		})
		tick := func(at time.Time) int {
			now = at
			n, err := s.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}

		if n := tick(t0); n != 0 {
			t.Errorf("30h out: %d reminders", n)
		}
		if n := tick(due.Add(-23 * time.Hour)); n != 1 {
			t.Errorf("23h out: %d reminders, want the 24h one", n)
		}
		if n := tick(due.Add(-2 * time.Hour)); n != 0 {
			t.Errorf("2h out: %d reminders, want none", n)
		}
		if n := tick(due.Add(-50 * time.Minute)); n != 1 {
			t.Errorf("50m out: %d reminders, want the 1h one", n)
		}
		if n := tick(due.Add(-10 * time.Minute)); n != 0 {
			t.Errorf("10m out: %d reminders, want none", n)
		}
		if woken != 2 {
			t.Errorf("relay woken %d times, want 2", woken)
		}

		keys := f.reminders(t, task.ID, domain.EventTaskDueSoon)
		if len(keys) != 2 || keys[0] == keys[1] {
			t.Fatalf("due-soon keys = %v", keys)
		}

		// Moving the deadline clears the marker; the new deadline is reminded.
		newDue := due.Add(48 * time.Hour)
		if _, err := f.svc.UpdateTask(ctx, domain.User("usr_alice"), tenant, task.ID, 1, domain.Patch{DueAt: domain.SetTime(newDue)}); err != nil {
			t.Fatal(err)
		}
		if n := tick(newDue.Add(-20 * time.Hour)); n != 1 {
			t.Errorf("after reschedule: %d reminders, want 1", n)
		}
		if keys := f.reminders(t, task.ID, domain.EventTaskDueSoon); len(keys) != 3 {
			t.Errorf("due-soon keys after reschedule = %d, want 3", len(keys))
		}
	})
}

func TestSchedulerSkipsClosedAndHandlesUnassigned(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newSchedFixture(t, b)
		done := f.task(t, t0.Add(-time.Hour), "usr_bob")
		if _, err := f.svc.CompleteTask(ctx, domain.User("usr_bob"), tenant, done.ID, nil); err != nil {
			t.Fatal(err)
		}
		unassigned := f.task(t, t0.Add(-time.Hour), "")

		now := t0
		s := f.scheduler(&now)
		if n, err := s.Tick(ctx); err != nil || n != 1 {
			t.Fatalf("tick = %d, %v; want only the unassigned task claimed", n, err)
		}
		if keys := f.reminders(t, unassigned.ID, domain.EventTaskOverdue); len(keys) != 0 {
			t.Errorf("an unassigned task produced reminders: %v", keys)
		}
		// It was still marked, so it is not rescanned.
		if n, _ := s.Tick(ctx); n != 0 {
			t.Errorf("second tick claimed %d tasks", n)
		}
		if keys := f.reminders(t, done.ID, domain.EventTaskOverdue); len(keys) != 0 {
			t.Errorf("a completed task was reminded")
		}
	})
}

func TestSchedulerRunStopsOnCancel(t *testing.T) {
	b := repotest.Memory()
	f := newSchedFixture(t, b)
	f.task(t, time.Now().Add(-time.Hour), "usr_bob")
	runCtx, cancel := context.WithCancel(ctx)
	s := NewScheduler(b.Tx, b.Tasks, b.Outbox, SchedulerConfig{Interval: time.Hour, Logger: quiet})
	done := make(chan error)
	go func() { done <- s.Run(runCtx) }()
	waitFor(t, "first tick", func() bool { return count(t, b, domain.OutboxPending) == 2 }) // task.created + overdue
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}
}

// --- cleanup ---------------------------------------------------------------

func TestCleanup(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		now := t0.Add(30 * 24 * time.Hour)
		rows := seedOutbox(t, b, 6, 10, t0)
		claimed, _ := b.Outbox.ClaimBatch(ctx, t0, 10)
		ages := map[uuid.UUID]time.Duration{} // dispatch age per row
		for i, e := range claimed {
			switch i {
			case 0, 1, 2:
				ages[e.ID] = 8 * 24 * time.Hour
			case 3:
				ages[e.ID] = 6 * 24 * time.Hour
			default:
				continue // left in processing
			}
			if err := b.Outbox.MarkDispatched(ctx, e.ID, *e.ClaimedAt, now.Add(-ages[e.ID])); err != nil {
				t.Fatal(err)
			}
		}
		_ = rows

		if _, err := b.Deliveries.Record(ctx, "old-sig", now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Deliveries.Record(ctx, "new-sig", now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		c := NewCleanup(b.Outbox, CleanupConfig{BatchSize: 2, Deliveries: b.Deliveries, Now: func() time.Time { return now }, Logger: quiet})
		n, err := c.RunOnce(ctx)
		if err != nil || n != 4 {
			t.Fatalf("RunOnce = %d, %v; want 3 outbox rows and 1 delivery deleted", n, err)
		}
		if fresh, _ := b.Deliveries.Record(ctx, "new-sig", now); fresh {
			t.Error("a delivery inside the replay window was deleted")
		}
		if got := count(t, b, domain.OutboxDispatched); got != 1 {
			t.Errorf("dispatched rows left = %d, want the 6-day-old one", got)
		}
		if got := count(t, b, domain.OutboxProcessing); got != 2 {
			t.Errorf("processing rows left = %d, want 2", got)
		}

		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error)
		go func() { done <- c.Run(runCtx) }()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v", err)
		}
	})
}
