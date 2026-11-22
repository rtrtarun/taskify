package worker

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// queuedLearning claims every pending learning job and returns their dedup
// keys, sorted.
func queuedLearning(t *testing.T, b repotest.Backend, now time.Time) []string {
	t.Helper()
	jobs, err := b.AgentJobs.Claim(ctx, now, 100, []string{domain.AgentLearning})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, j := range jobs {
		in, err := domain.ParseLearningInput(j.InputContext)
		if err != nil || j.TaskID != nil || in.Week != domain.LearningWeek(now) {
			t.Errorf("learning job = %+v (input %s, %v)", j, j.InputContext, err)
		}
		keys = append(keys, j.DedupKey+"@"+in.WorkspaceID)
	}
	slices.Sort(keys)
	return keys
}

// TestLearningOnePerTenantPerWeek: the weekly scan queues one learning job
// per tenant with a workspace that allows the learning agent, however many
// scans run that week, on however many replicas, and one more the next week
// (G16).
func TestLearningOnePerTenantPerWeek(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		yes := true
		for _, w := range []struct {
			tenant, id string
			p          domain.AgentSettingsPatch
		}{
			{tenant, "ws_b", domain.AgentSettingsPatch{AgentsEnabled: &yes}},
			{tenant, "ws_a", domain.AgentSettingsPatch{AgentsEnabled: &yes}},
			{"tenant_off", "ws", domain.AgentSettingsPatch{}},
			{"tenant_nolearn", "ws", domain.AgentSettingsPatch{AgentsEnabled: &yes, DisabledAgents: &[]string{domain.AgentLearning}}},
			{"tenant_two", "ws", domain.AgentSettingsPatch{AgentsEnabled: &yes}},
		} {
			ws, err := domain.NewWorkspace(w.tenant, w.id, w.id, t0)
			if err != nil {
				t.Fatal(err)
			}
			if err := ws.UpdateAgentSettings(w.p, t0); err != nil {
				t.Fatal(err)
			}
			if err := b.Workspaces.Create(ctx, ws); err != nil {
				t.Fatal(err)
			}
		}
		monday := time.Date(2026, 11, 23, 0, 30, 0, 0, time.UTC)
		now := monday
		var mu sync.Mutex
		clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		scheduler := func() *Scheduler {
			return NewScheduler(b.Tx, b.Tasks, b.Outbox, SchedulerConfig{BatchSize: 1, Logger: quiet, Now: clock,
				Jobs: b.AgentJobs, Workspaces: b.Workspaces, Learning: true})
		}
		a, c := scheduler(), scheduler()
		var wg sync.WaitGroup
		for _, s := range []*Scheduler{a, c, a, c} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Tick(ctx); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		// A batch both replicas saw may be left to the next tick.
		for _, s := range []*Scheduler{a, c} {
			if _, err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"learning:tenant_acme:week:2026-W48@ws_a", "learning:tenant_two:week:2026-W48@ws"}
		if got := queuedLearning(t, b, monday); !slices.Equal(got, want) {
			t.Errorf("week 48 = %v, want %v", got, want)
		}
		// Later that week: nothing more.
		mu.Lock()
		now = monday.Add(5 * 24 * time.Hour)
		mu.Unlock()
		if _, err := scheduler().Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := queuedLearning(t, b, now); len(got) != 0 {
			t.Errorf("later in week 48 = %v", got)
		}
		// The next week: one more each.
		mu.Lock()
		now = monday.Add(7 * 24 * time.Hour)
		mu.Unlock()
		if _, err := a.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := queuedLearning(t, b, now); len(got) != 2 || !strings.Contains(got[0], "2026-W49") {
			t.Errorf("week 49 = %v", got)
		}
	})
}

// TestLearningOff: without Learning, or without the workspace repository,
// no learning job is queued.
func TestLearningOff(t *testing.T) {
	b := repotest.Memory()
	yes := true
	ws, _ := domain.NewWorkspace(tenant, "ws", "W", t0)
	if err := ws.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: &yes}, t0); err != nil {
		t.Fatal(err)
	}
	if err := b.Workspaces.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []SchedulerConfig{
		{Jobs: b.AgentJobs, Workspaces: b.Workspaces},
		{Jobs: b.AgentJobs, Learning: true},
	} {
		cfg.Logger, cfg.Now = quiet, func() time.Time { return t0 }
		if _, err := NewScheduler(b.Tx, b.Tasks, b.Outbox, cfg).Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := queuedLearning(t, b, t0); len(got) != 0 {
		t.Errorf("queued = %v", got)
	}
}

// learningRow writes a learning.suggestion outbox row with payload.
func learningRow(t *testing.T, b repotest.Backend, payload string) domain.OutboxEntry {
	t.Helper()
	docs := "docs"
	e, err := domain.NewLearningSuggestionEntry(domain.LearningSuggestion{TenantID: tenant, JobID: uuid.New(),
		Pattern:            domain.EventPattern{EventType: domain.EventTaskCommentAdded, Label: &docs},
		ProposedAdjustment: -10, SignalCount: 6, Rationale: "Dismissed by three people."}, 5, t0)
	if err != nil {
		t.Fatal(err)
	}
	if payload != "" {
		e.Payload = json.RawMessage(payload)
	}
	if _, err := b.Outbox.Insert(ctx, []domain.OutboxEntry{e}); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestRelaySubmitsLearningSuggestions: a learning.suggestion row is sent to
// POST /v1/learning/suggestions with its body as written; a row AgentPulse
// refuses, or one the relay cannot read, goes to dead letter at once.
func TestRelaySubmitsLearningSuggestions(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		r := NewRelay(b.Outbox, client, RelayConfig{Logger: quiet})
		sent := learningRow(t, b, "")
		if n, err := r.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("run: %d, %v", n, err)
		}
		reqs := mock.RequestsTo("POST", "/v1/learning/suggestions")
		want := `{"tenantId":"tenant_acme","eventPattern":{"eventType":"task.comment_added","label":"docs"},` +
			`"currentAdjustment":0,"proposedAdjustment":-10,"signalCount":6,"rationale":"Dismissed by three people."}`
		if len(reqs) != 1 || strings.TrimSpace(string(reqs[0].Body)) != want {
			t.Fatalf("requests = %d, body %s", len(reqs), reqs[0].Body)
		}
		if sgs := mock.Suggestions(); len(sgs) != 1 || sgs[0].Status != "PENDING_APPROVAL" {
			t.Errorf("suggestions = %+v", sgs)
		}
		if len(mock.RequestsTo("POST", "/v1/notify")) != 0 {
			t.Error("a learning row was sent as a notification")
		}
		rows, _ := b.Outbox.ListForAggregate(ctx, sent.AggregateID)
		if len(rows) != 1 || rows[0].Status != domain.OutboxDispatched {
			t.Errorf("row = %+v", rows)
		}

		refused := learningRow(t, b, `{"tenantId":"tenant_acme","eventPattern":{"eventType":"task.updated","label":null},`+
			`"currentAdjustment":0,"proposedAdjustment":-30,"signalCount":6,"rationale":"r"}`)
		other := learningRow(t, b, `{"tenantId":"tenant_other","eventPattern":{"eventType":"task.updated","label":null},`+
			`"currentAdjustment":0,"proposedAdjustment":-5,"signalCount":6,"rationale":"r"}`)
		if n, err := r.RunOnce(ctx); err != nil || n != 2 {
			t.Fatalf("second run: %d, %v", n, err)
		}
		for name, e := range map[string]domain.OutboxEntry{"refused": refused, "another tenant": other} {
			rows, _ := b.Outbox.ListForAggregate(ctx, e.AggregateID)
			if len(rows) != 1 || rows[0].Status != domain.OutboxDeadLetter || rows[0].Attempts != 1 {
				t.Errorf("%s: %+v", name, rows)
			}
		}
		if n := len(mock.RequestsTo("POST", "/v1/learning/suggestions")); n != 2 {
			t.Errorf("suggestion requests = %d, want 2: the row naming another tenant is never sent", n)
		}
	})
}
