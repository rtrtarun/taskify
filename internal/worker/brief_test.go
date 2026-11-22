package worker

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
)

// queuedBriefs claims every pending digest job and returns their dedup
// keys, sorted; each call sees only the briefs queued since the last.
func queuedBriefs(t *testing.T, b repotest.Backend, now time.Time) []string {
	t.Helper()
	jobs, err := b.AgentJobs.Claim(ctx, now, 100, []string{domain.AgentDigest})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, j := range jobs {
		in, err := domain.ParseBriefInput(j.InputContext)
		if err != nil || j.TaskID != nil || in.WorkspaceID != "ws" || in.At.Hour() != 8 {
			t.Errorf("brief job = %+v (input %s, %v)", j, j.InputContext, err)
		}
		keys = append(keys, j.DedupKey)
	}
	slices.Sort(keys)
	return keys
}

// TestMorningBriefOnePerUserPerDay is the M10-02 test "one brief per user
// per day", with two schedulers as two replicas, and the scheduler's half
// of "an empty brief is skipped": a person with no open task gets no job.
func TestMorningBriefOnePerUserPerDay(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		svc := service.NewTaskService(
			service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments, Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs},
			service.Config{PublicBaseURL: "https://taskify.test"},
			service.WithClock(func() time.Time { return t0 }),
		)
		for _, ws := range []struct {
			id string
			on bool
		}{{"ws", true}, {"off", false}} {
			w, err := svc.CreateWorkspace(ctx, tenant, ws.id, ws.id)
			if err != nil {
				t.Fatal(err)
			}
			on := ws.on
			if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: &on}, t0); err != nil {
				t.Fatal(err)
			}
			if err := b.Workspaces.Update(ctx, w); err != nil {
				t.Fatal(err)
			}
		}
		alice := domain.User("usr_alice")
		create := func(ws, assignee string, status domain.Status) {
			task, err := svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: tenant, WorkspaceID: ws, Title: "Work", AssigneeID: assignee})
			if err != nil {
				t.Fatal(err)
			}
			if status != domain.StatusTodo {
				if _, err := svc.UpdateTask(ctx, alice, tenant, task.ID, task.Version, domain.Patch{Status: &status}); err != nil {
					t.Fatal(err)
				}
			}
		}
		create("ws", "usr_bob", domain.StatusTodo)
		create("ws", "usr_bob", domain.StatusInProgress)
		create("ws", "usr_carol", domain.StatusInProgress)
		create("ws", "usr_dave", domain.StatusCompleted) // no open task: no brief
		create("off", "usr_erin", domain.StatusTodo)     // agents off there: no brief

		day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
		now := day.Add(7*time.Hour + 59*time.Minute)
		var mu sync.Mutex
		clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		set := func(at time.Time) { mu.Lock(); now = at; mu.Unlock() }
		sched := func() *Scheduler {
			return NewScheduler(b.Tx, b.Tasks, b.Outbox, SchedulerConfig{
				BatchSize: 1, Logger: quiet, Jobs: b.AgentJobs, Now: clock, Briefs: true, BriefHourUTC: 8,
			})
		}
		a, c := sched(), sched()

		// Before the brief hour: nothing.
		if _, err := a.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := queuedBriefs(t, b, now); len(got) != 0 {
			t.Fatalf("briefs before 08:00 = %v", got)
		}

		// At 08:00 both replicas scan, twice each, at the same time.
		set(day.Add(8 * time.Hour))
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
		want := []string{
			domain.BriefJobDedupKey(tenant, "usr_bob", day),
			domain.BriefJobDedupKey(tenant, "usr_carol", day),
		}
		if got := queuedBriefs(t, b, now); !slices.Equal(got, want) {
			t.Fatalf("briefs at 08:00 = %v, want %v", got, want)
		}

		// Later that day, and after a restart: nothing new.
		set(day.Add(15 * time.Hour))
		for _, s := range []*Scheduler{a, sched()} {
			if _, err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if got := queuedBriefs(t, b, now); len(got) != 0 {
			t.Errorf("briefs later the same day = %v", got)
		}

		// The next morning: one more each.
		next := day.Add(24 * time.Hour)
		set(next.Add(8*time.Hour + 30*time.Minute))
		if _, err := c.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		want = []string{
			domain.BriefJobDedupKey(tenant, "usr_bob", next),
			domain.BriefJobDedupKey(tenant, "usr_carol", next),
		}
		if got := queuedBriefs(t, b, now); !slices.Equal(got, want) {
			t.Errorf("briefs the next day = %v, want %v", got, want)
		}
	})
}

func TestMorningBriefOff(t *testing.T) {
	b := repotest.Memory()
	f := newSchedFixture(t, b)
	w, _ := b.Workspaces.Get(ctx, tenant, "ws")
	on := true
	_ = w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: &on}, t0)
	if err := b.Workspaces.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	f.task(t, t0.Add(30*24*time.Hour), "usr_bob")
	now := t0.Add(24 * time.Hour)
	// Briefs need both the switch and somewhere to queue jobs.
	for _, cfg := range []SchedulerConfig{{Jobs: b.AgentJobs}, {Briefs: true}} {
		cfg.Logger, cfg.Now = quiet, func() time.Time { return now }
		if _, err := NewScheduler(b.Tx, b.Tasks, b.Outbox, cfg).Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := queuedBriefs(t, b, now); len(got) != 0 {
		t.Errorf("briefs with the brief scan off = %v", got)
	}
}
