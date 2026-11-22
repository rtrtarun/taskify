package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
	"github.com/rtrtarun/taskify/internal/worker"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

func label(s string) *string { return &s }

// describePatterns renders patterns for comparison: scope, signals, ratios.
func describePatterns(ps []domain.LearningPattern) string {
	var lines []string
	for _, p := range ps {
		lines = append(lines, fmt.Sprintf("%s: %d signals, dismiss %v, mute %v", p.Pattern(), p.SignalCount, p.DismissRatio, p.MuteRatio))
	}
	return strings.Join(lines, "\n")
}

// TestFindPatternsFindsAConsistentlyDismissedLabel is M11-02: in a history
// where comments on docs tasks are dismissed again and again by several
// people, the docs label is a pattern; labels people act on are not.
func TestFindPatternsFindsAConsistentlyDismissedLabel(t *testing.T) {
	windowStart := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	a := domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{
		{EventType: "task.comment_added", Label: label("backend"), Delivered: 40, Acted: 25, Dismissed: 3, DistinctUsers: 6},
		{EventType: "task.comment_added", Label: label("docs"), Delivered: 20, Acted: 1, Dismissed: 14, Muted: 2, DistinctUsers: 4},
		{EventType: "task.assigned", Label: label("docs"), Delivered: 6, Acted: 6, DistinctUsers: 3},
	}}
	got, err := FindPatterns(a, windowStart)
	if err != nil {
		t.Fatal(err)
	}
	if want := "task.comment_added, label docs: 16 signals, dismiss 0.7, mute 0.1"; describePatterns(got) != want {
		t.Fatalf("patterns:\n%s\nwant:\n%s", describePatterns(got), want)
	}
	p := got[0]
	if p.Delivered != 20 || p.Acted != 1 || p.Dismissed != 14 || p.Muted != 2 || p.DistinctUsers != 4 || p.ID != "" ||
		p.CurrentAdjustment != 0 {
		t.Errorf("pattern = %+v", p)
	}
	// The pattern does not share the analytics' label.
	*a.Groups[1].Label = "changed"
	if *p.Label != "docs" {
		t.Error("the pattern shares its label with the input")
	}
}

// TestFindPatternsDropsWhatIsBelowAThreshold: below 5 signals, 2 users, or a
// consistent share, or outside the 30-day window, nothing is proposed.
func TestFindPatternsDropsWhatIsBelowAThreshold(t *testing.T) {
	windowStart := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	for name, g := range map[string]domain.FeedbackGroup{
		"four signals":            {EventType: "task.updated", Label: label("docs"), Delivered: 4, Dismissed: 3, Muted: 1, DistinctUsers: 3},
		"one user":                {EventType: "task.updated", Label: label("docs"), Delivered: 12, Dismissed: 12, DistinctUsers: 1},
		"no users":                {EventType: "task.updated", Label: label("docs"), Delivered: 12, Dismissed: 12},
		"under half":              {EventType: "task.updated", Label: label("docs"), Delivered: 100, Dismissed: 30, Muted: 19, DistinctUsers: 9},
		"snoozes are not signals": {EventType: "task.updated", Label: label("docs"), Delivered: 10, Dismissed: 4, Snoozed: 6, DistinctUsers: 3},
		"nothing delivered":       {EventType: "task.updated", Label: label("docs"), DistinctUsers: 3},
		"no event type":           {Label: label("docs"), Delivered: 10, Dismissed: 10, DistinctUsers: 3},
	} {
		got, err := FindPatterns(domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{g}}, windowStart)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %s, %v", name, describePatterns(got), err)
		}
	}

	// Exactly at every threshold is a pattern.
	edge := domain.FeedbackGroup{EventType: "task.updated", Delivered: 10, Dismissed: 4, Muted: 1, DistinctUsers: 2}
	if got, err := FindPatterns(domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{edge}}, windowStart); err != nil ||
		describePatterns(got) != "task.updated, no label: 5 signals, dismiss 0.4, mute 0.1" {
		t.Errorf("at the thresholds: %s, %v", describePatterns(got), err)
	}

	// Counts from before the window are not used: an answer for a longer
	// window is refused, as is one that does not say when it starts.
	strong := domain.FeedbackGroup{EventType: "task.updated", Label: label("docs"), Delivered: 50, Dismissed: 50, DistinctUsers: 9}
	for name, since := range map[string]time.Time{"31 days": windowStart.Add(-24 * time.Hour), "no since": {}} {
		got, err := FindPatterns(domain.FeedbackAnalytics{Since: since, Groups: []domain.FeedbackGroup{strong}}, windowStart)
		if !errors.Is(err, ErrAnalyticsWindow) || len(got) != 0 {
			t.Errorf("%s: %s, %v", name, describePatterns(got), err)
		}
	}
	// A window start with sub-second precision is asked for to the second.
	if _, err := FindPatterns(domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{strong}},
		windowStart.Add(400*time.Millisecond)); err != nil {
		t.Errorf("same second: %v", err)
	}
}

// TestFindPatternsOrder: the most signals first, then the larger share
// dismissed or muted, then event type and label, no label first; a scope
// listed twice counts once; deliveries fewer than signals use the signals.
func TestFindPatternsOrder(t *testing.T) {
	windowStart := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	a := domain.FeedbackAnalytics{Since: windowStart.Add(time.Hour), Groups: []domain.FeedbackGroup{
		{EventType: "task.updated", Label: label("ops"), Delivered: 10, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.updated", Label: label("docs"), Delivered: 10, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.updated", Delivered: 10, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.created", Label: label("docs"), Delivered: 8, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.comment_added", Label: label("docs"), Delivered: 30, Dismissed: 20, Muted: 5, DistinctUsers: 5},
		{EventType: "task.updated", Label: label("docs"), Delivered: 99, Dismissed: 99, DistinctUsers: 9},
		{EventType: "task.due_soon", Label: label("x"), Delivered: 0, Dismissed: 5, Acted: 1, DistinctUsers: 2},
	}}
	got, err := FindPatterns(a, windowStart)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"task.comment_added, label docs: 25 signals, dismiss 0.667, mute 0.167",
		"task.created, label docs: 6 signals, dismiss 0.75, mute 0",
		"task.updated, no label: 6 signals, dismiss 0.6, mute 0",
		"task.updated, label docs: 6 signals, dismiss 0.6, mute 0",
		"task.updated, label ops: 6 signals, dismiss 0.6, mute 0",
		"task.due_soon, label x: 5 signals, dismiss 0.833, mute 0",
	}, "\n")
	if describePatterns(got) != want {
		t.Errorf("patterns:\n%s\nwant:\n%s", describePatterns(got), want)
	}
	// The same input gives the same answer.
	again, _ := FindPatterns(a, windowStart)
	if describePatterns(again) != describePatterns(got) {
		t.Error("FindPatterns is not deterministic")
	}
}

// learningFixture runs the real learning agent against the mock AgentPulse,
// through the real client, with the model scripted.
type learningFixture struct {
	*fixture
	pulse  *mockagentpulse.Server
	client *agentpulse.Client
	// at is the learning agent's fixed clock, so the job's week never
	// turns while a test runs.
	at  time.Time
	seq int
}

func newLearningFixture(t *testing.T, b repotest.Backend, hitl bool) *learningFixture {
	t.Helper()
	f := newFixture(t, b, hitl)
	pulse := mockagentpulse.New()
	srv := httptest.NewServer(pulse)
	t.Cleanup(srv.Close)
	lf := &learningFixture{fixture: f, pulse: pulse, client: agentpulse.New(srv.URL, "k", agentpulse.WithBreaker(nil)),
		at: f.clock.now().UTC()}
	if err := f.reg.Register(tools.ProposeNotificationRule{
		Suggestions: service.NewLearningService(b.Tx, b.Learning, b.Outbox, 5, f.clock.now, nil)}); err != nil {
		t.Fatal(err)
	}
	return lf
}

func (lf *learningFixture) learning() Learning {
	return Learning{Source: AgentPulseLearning{Client: lf.client}, Record: lf.b.Learning, Now: func() time.Time { return lf.at }}
}

func (lf *learningFixture) runtime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(Deps{Tx: lf.b.Tx, Outbox: lf.b.Outbox, Jobs: lf.b.AgentJobs, Tasks: lf.b.Tasks, Workspaces: lf.b.Workspaces,
		LLM: lf.mock, Tools: lf.reg}, Config{Logger: quiet, Now: lf.clock.now, Workers: 1}, lf.learning())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// queue queues the tenant's learning job for the week of at.
func (lf *learningFixture) queue(t *testing.T, at time.Time) *domain.AgentJob {
	t.Helper()
	job, err := domain.NewLearningJob(domain.LearningTenant{TenantID: "tenant_acme", WorkspaceID: "ws"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := lf.b.AgentJobs.Insert(ctx, job); err != nil || !ok {
		t.Fatalf("queue: %v, %v", ok, err)
	}
	return &job
}

// run claims and runs the queued jobs.
func (lf *learningFixture) run(t *testing.T) {
	t.Helper()
	if _, err := lf.runtime(t).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
}

// history makes AgentPulse hold one notification per signal, of eventType
// with labels, to the signal's user, and that user's reaction to it.
func (lf *learningFixture) history(t *testing.T, eventType string, labels []any, signals ...[2]string) {
	t.Helper()
	for _, s := range signals {
		lf.seq++
		id := fmt.Sprintf("ntf_h%03d", lf.seq)
		lf.pulse.SeedNotification(id, map[string]any{"tenantId": "tenant_acme", "userId": s[0], "eventType": eventType,
			"payload": map[string]any{"taskId": uuid.NewString(), "labels": labels}})
		if err := lf.client.RecordFeedback(ctx, agentpulse.Feedback{NotificationID: id, TenantID: "tenant_acme", UserID: s[0],
			Signal: s[1]}); err != nil {
			t.Fatal(err)
		}
	}
}

// docsDismissed is a history in which comments on docs tasks are dismissed
// or muted by three people, and comments on backend tasks are acted on.
func (lf *learningFixture) docsDismissed(t *testing.T) {
	t.Helper()
	lf.history(t, domain.EventTaskCommentAdded, []any{"docs"},
		[2]string{"usr_1", "dismissed"}, [2]string{"usr_1", "dismissed"}, [2]string{"usr_2", "dismissed"},
		[2]string{"usr_2", "muted"}, [2]string{"usr_3", "dismissed"}, [2]string{"usr_3", "dismissed"}, [2]string{"usr_3", "acted"})
	lf.history(t, domain.EventTaskCommentAdded, []any{"backend"},
		[2]string{"usr_1", "acted"}, [2]string{"usr_2", "acted"}, [2]string{"usr_3", "acted"}, [2]string{"usr_1", "dismissed"})
}

func (lf *learningFixture) relay(t *testing.T) {
	t.Helper()
	if _, err := worker.NewRelay(lf.b.Outbox, lf.client, worker.RelayConfig{Logger: quiet}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
}

func propose(pattern string, adjustment int) map[string]any {
	return map[string]any{"pattern_id": pattern, "proposed_adjustment": adjustment,
		"rationale": "Most of these notifications were dismissed or muted by several people."}
}

// TestLearningProposesOnADismissedLabel covers M11's exit criteria: a
// synthetic history in which one label is dismissed consistently yields a
// proposal naming that label; it is submitted to AgentPulse, where it waits
// as PENDING_APPROVAL; and nothing takes effect without an admin's decision.
// The workspace requires approval for everything, as by default: the
// suggestion is not held in Taskify as well.
func TestLearningProposesOnADismissedLabel(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		lf := newLearningFixture(t, b, true)
		lf.docsDismissed(t)
		job := lf.queue(t, lf.at)
		lf.mock.Then(llm.Call("c1", tools.ProposeNotificationRuleName, propose("p1", -10)),
			llm.Text("Proposed lowering comments on docs tasks by 10."))
		lf.run(t)

		// The model saw the docs pattern, and only it.
		reqs := lf.mock.Requests()
		if len(reqs) != 2 {
			t.Fatalf("model calls = %d", len(reqs))
		}
		facts := reqs[0].Messages[1].Content
		if !strings.Contains(facts, `"label": "docs"`) || !strings.Contains(facts, `"pattern_id": "p1"`) ||
			!strings.Contains(facts, `"signal_count": 6`) || strings.Contains(facts, "backend") {
			t.Errorf("facts:\n%s", facts)
		}
		got := lf.job(t, job.ID)
		if got.Status != domain.JobCompleted {
			t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
		}
		var result struct {
			Executed []struct {
				Tool string `json:"tool"`
			} `json:"executed"`
			Report struct {
				Week     string                   `json:"week"`
				Patterns []domain.LearningPattern `json:"patterns"`
				Summary  string                   `json:"summary"`
			} `json:"report"`
		}
		if err := json.Unmarshal(got.ExecutionResult, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Executed) != 1 || result.Report.Week != domain.LearningWeek(lf.at) || len(result.Report.Patterns) != 1 ||
			*result.Report.Patterns[0].Label != "docs" || !strings.Contains(result.Report.Summary, "docs") {
			t.Errorf("execution result = %s", got.ExecutionResult)
		}

		// Nothing reaches AgentPulse until the relay sends the row; then it
		// waits there for an admin.
		if n := len(lf.pulse.Suggestions()); n != 0 {
			t.Fatalf("suggestions before the relay = %d", n)
		}
		lf.relay(t)
		sgs := lf.pulse.Suggestions()
		if len(sgs) != 1 {
			t.Fatalf("suggestions = %+v", sgs)
		}
		sg := sgs[0]
		pattern := sg.Body["eventPattern"].(map[string]any)
		if sg.Status != "PENDING_APPROVAL" || pattern["eventType"] != domain.EventTaskCommentAdded || pattern["label"] != "docs" ||
			sg.Body["proposedAdjustment"] != -10.0 || sg.Body["currentAdjustment"] != 0.0 || sg.Body["signalCount"] != 6.0 ||
			sg.Body["tenantId"] != "tenant_acme" {
			t.Errorf("suggestion = %+v", sg)
		}

		// Nothing took effect: Taskify registered no rule and sent no
		// notification, and the suggestion is undecided.
		if n := len(lf.pulse.RequestsTo("POST", "/v1/rules")) + len(lf.pulse.RequestsTo("POST", "/v1/notify")); n != 0 {
			t.Errorf("AgentPulse writes besides the suggestion = %d", n)
		}
		if rows, _ := lf.b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 1 || rows[0].Status != domain.OutboxDispatched {
			t.Errorf("outbox = %+v", rows)
		}
		// An admin's decision, made through the admin proxy's call, is
		// what changes it.
		if _, err := lf.client.ReviewLearningSuggestion(ctx, "tenant_acme", sg.ID, "approve", "usr_admin"); err != nil {
			t.Fatal(err)
		}
		if st := lf.pulse.Suggestions()[0].Status; st != "APPROVED" {
			t.Errorf("after the admin's approval: %s", st)
		}
	})
}

// TestLearningBelowThresholdsProposesNothing: a pattern below 5 signals, 2
// users, or a consistent share, or older than the 30-day window, yields no
// proposal: the job is skipped and the model is not called.
func TestLearningBelowThresholdsProposesNothing(t *testing.T) {
	dismissals := func(users ...string) [][2]string {
		var out [][2]string
		for _, u := range users {
			out = append(out, [2]string{u, "dismissed"})
		}
		return out
	}
	for name, setup := range map[string]func(t *testing.T, lf *learningFixture){
		"four signals": func(t *testing.T, lf *learningFixture) {
			lf.history(t, domain.EventTaskUpdated, []any{"docs"}, dismissals("usr_1", "usr_2", "usr_1", "usr_2")...)
		},
		"one user": func(t *testing.T, lf *learningFixture) {
			lf.history(t, domain.EventTaskUpdated, []any{"docs"}, dismissals("usr_1", "usr_1", "usr_1", "usr_1", "usr_1", "usr_1")...)
		},
		"mostly acted on": func(t *testing.T, lf *learningFixture) {
			lf.history(t, domain.EventTaskUpdated, []any{"docs"}, dismissals("usr_1", "usr_2", "usr_1", "usr_2", "usr_3")...)
			lf.history(t, domain.EventTaskUpdated, []any{"docs"}, [2]string{"usr_1", "acted"}, [2]string{"usr_2", "acted"},
				[2]string{"usr_3", "acted"}, [2]string{"usr_1", "acted"}, [2]string{"usr_2", "acted"}, [2]string{"usr_3", "acted"})
		},
		"older than 30 days": func(t *testing.T, lf *learningFixture) {
			lf.pulse.SetClock(func() time.Time { return lf.at.Add(-31 * 24 * time.Hour) })
			lf.history(t, domain.EventTaskUpdated, []any{"docs"}, dismissals("usr_1", "usr_2", "usr_3", "usr_1", "usr_2", "usr_3")...)
			lf.pulse.SetClock(nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			lf := newLearningFixture(t, repotest.Memory(), false)
			setup(t, lf)
			job := lf.queue(t, lf.at)
			lf.run(t)
			got := lf.job(t, job.ID)
			if got.Status != domain.JobCompleted || !strings.Contains(string(got.ExecutionResult), `"stopped":"skipped"`) {
				t.Errorf("job = %s %s", got.Status, got.ExecutionResult)
			}
			if n := len(lf.mock.Requests()); n != 0 {
				t.Errorf("the model was called %d times", n)
			}
			if rows, _ := lf.b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 0 {
				t.Errorf("outbox = %v", rows)
			}
			if got, _ := lf.b.Learning.PatternsSince(ctx, "tenant_acme", lf.at.Add(-time.Hour)); len(got) != 0 {
				t.Errorf("recorded = %v", got)
			}
		})
	}
}

// TestLearningRejectsOutOfRangeAdjustments: an adjustment outside -20 to 20
// is refused before anything is recorded; the model may correct it once,
// and a second refusal fails the job with nothing submitted.
func TestLearningRejectsOutOfRangeAdjustments(t *testing.T) {
	t.Run("corrected", func(t *testing.T) {
		lf := newLearningFixture(t, repotest.Memory(), false)
		lf.docsDismissed(t)
		job := lf.queue(t, lf.at)
		lf.mock.Then(llm.Call("c1", tools.ProposeNotificationRuleName, propose("p1", -25)),
			llm.Call("c2", tools.ProposeNotificationRuleName, propose("p1", -12)), llm.Text("Proposed -12."))
		lf.run(t)
		if got := lf.job(t, job.ID); got.Status != domain.JobCompleted {
			t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
		}
		if outcomes := lf.outcomes(t, job.ID); !slices.Equal(outcomes, []string{OutcomeRejected, OutcomeToolCall, OutcomeFinal}) {
			t.Errorf("outcomes = %v", outcomes)
		}
		lf.relay(t)
		if sgs := lf.pulse.Suggestions(); len(sgs) != 1 || sgs[0].Body["proposedAdjustment"] != -12.0 {
			t.Errorf("suggestions = %+v", sgs)
		}
	})
	t.Run("refused twice", func(t *testing.T) {
		lf := newLearningFixture(t, repotest.Memory(), false)
		lf.docsDismissed(t)
		job := lf.queue(t, lf.at)
		lf.mock.Then(llm.Call("c1", tools.ProposeNotificationRuleName, propose("p1", 25)),
			llm.Call("c2", tools.ProposeNotificationRuleName, propose("p1", -40)))
		lf.run(t)
		if got := lf.job(t, job.ID); got.Status != domain.JobFailed {
			t.Errorf("job = %s", got.Status)
		}
		if rows, _ := lf.b.Outbox.ListForAggregate(ctx, job.ID); len(rows) != 0 {
			t.Errorf("outbox = %v", rows)
		}
		if got, _ := lf.b.Learning.PatternsSince(ctx, "tenant_acme", lf.at.Add(-time.Hour)); len(got) != 0 {
			t.Errorf("recorded = %v", got)
		}
	})
}

// TestLearningRetrySubmitsOnce: a run that submitted a suggestion and then
// failed is run again without that pattern, so AgentPulse is asked once.
func TestLearningRetrySubmitsOnce(t *testing.T) {
	lf := newLearningFixture(t, repotest.Memory(), false)
	lf.docsDismissed(t)
	job := lf.queue(t, lf.at)
	lf.mock.Then(llm.Call("c1", tools.ProposeNotificationRuleName, propose("p1", -10)), llm.Fail(llm.ErrTimeout))
	lf.run(t)
	if got := lf.job(t, job.ID); got.Status != domain.JobPending || got.Attempts != 1 {
		t.Fatalf("after the failed attempt: %s, %d attempts", got.Status, got.Attempts)
	}
	lf.clock.advance(time.Minute) // past the 30-second backoff
	lf.run(t)
	got := lf.job(t, job.ID)
	if got.Status != domain.JobCompleted || !strings.Contains(string(got.ExecutionResult), "already has a suggestion") {
		t.Errorf("after the retry: %s %s", got.Status, got.ExecutionResult)
	}
	lf.relay(t)
	if n := len(lf.pulse.Suggestions()); n != 1 {
		t.Errorf("suggestions = %d, want 1", n)
	}
}

// TestLearningHistory: a pattern Taskify submitted within the window, or
// one with a suggestion pending at AgentPulse, is left out; the latest
// approved suggestion gives a pattern its current adjustment.
func TestLearningHistory(t *testing.T) {
	docs, ops, qa := "docs", "ops", "qa"
	at := func(l *string) domain.LearningPattern {
		return domain.LearningPattern{EventType: domain.EventTaskUpdated, Label: l, SignalCount: 6}
	}
	ps := []domain.LearningPattern{at(&docs), at(&ops), at(&qa), at(nil)}
	recent := []domain.EventPattern{{EventType: domain.EventTaskUpdated, Label: &docs}}
	pattern := func(l *string) *domain.EventPattern {
		return &domain.EventPattern{EventType: domain.EventTaskUpdated, Label: l}
	}
	suggestions := []SuggestionState{
		{Pattern: pattern(&ops), Status: agentpulse.SuggestionPending, ProposedAdjustment: -5},
		{Pattern: pattern(&qa), Status: agentpulse.SuggestionApproved, ProposedAdjustment: -4},
		{Pattern: pattern(&qa), Status: agentpulse.SuggestionRejected, ProposedAdjustment: -15},
		{Pattern: pattern(&qa), Status: agentpulse.SuggestionApproved, ProposedAdjustment: -8},
		{Status: agentpulse.SuggestionApproved, ProposedAdjustment: -20}, // a rule's
		{Pattern: &domain.EventPattern{EventType: domain.EventTaskCreated, Label: &qa}, Status: agentpulse.SuggestionPending},
	}
	got := withHistory(ps, recent, suggestions)
	if len(got) != 2 || *got[0].Label != "qa" || got[0].CurrentAdjustment != -8 || got[1].Label != nil || got[1].CurrentAdjustment != 0 {
		t.Errorf("withHistory = %+v", got)
	}

	// Through AgentPulse: an approved suggestion's adjustment reaches the
	// model and the submission.
	lf := newLearningFixture(t, repotest.Memory(), false)
	lf.docsDismissed(t)
	id := lf.pulse.SeedSuggestion(map[string]any{"tenantId": "tenant_acme", "proposedAdjustment": -6.0,
		"eventPattern": map[string]any{"eventType": domain.EventTaskCommentAdded, "label": "docs"}})
	if _, err := lf.client.ReviewLearningSuggestion(ctx, "tenant_acme", id, "approve", "usr_admin"); err != nil {
		t.Fatal(err)
	}
	lf.queue(t, lf.at)
	lf.mock.Then(llm.Call("c1", tools.ProposeNotificationRuleName, propose("p1", -12)), llm.Text("Lowered further."))
	lf.run(t)
	if facts := lf.mock.Requests()[0].Messages[1].Content; !strings.Contains(facts, `"current_adjustment": -6`) {
		t.Errorf("facts:\n%s", facts)
	}
	lf.relay(t)
	sgs := lf.pulse.Suggestions()
	if len(sgs) != 2 || sgs[1].Body["currentAdjustment"] != -6.0 || sgs[1].Body["proposedAdjustment"] != -12.0 {
		t.Errorf("suggestions = %+v", sgs)
	}

	// The next week the pattern rests: Taskify submitted it within the
	// window.
	lf.at = lf.at.Add(7 * 24 * time.Hour)
	next := lf.queue(t, lf.at)
	lf.run(t)
	if got := lf.job(t, next.ID); got.Status != domain.JobCompleted || !strings.Contains(string(got.ExecutionResult), "already has a suggestion") {
		t.Errorf("next week: %s %s", got.Status, got.ExecutionResult)
	}
}

// TestLearningJobEdges: a job for a past week is skipped without calling
// AgentPulse; analytics for a longer window fail the job; AgentPulse being
// down is an attempt, tried again later.
func TestLearningJobEdges(t *testing.T) {
	t.Run("out of date", func(t *testing.T) {
		lf := newLearningFixture(t, repotest.Memory(), false)
		job := lf.queue(t, lf.at.Add(-7*24*time.Hour))
		lf.run(t)
		if got := lf.job(t, job.ID); got.Status != domain.JobCompleted || !strings.Contains(string(got.ExecutionResult), "out of date") {
			t.Errorf("job = %s %s", got.Status, got.ExecutionResult)
		}
		if n := len(lf.pulse.Requests()); n != 0 {
			t.Errorf("AgentPulse calls = %d", n)
		}
	})
	t.Run("longer window", func(t *testing.T) {
		lf := newLearningFixture(t, repotest.Memory(), false)
		job := lf.queue(t, lf.at)
		l := lf.learning()
		l.Source = wideSource{}
		r, err := New(Deps{Tx: lf.b.Tx, Outbox: lf.b.Outbox, Jobs: lf.b.AgentJobs, Tasks: lf.b.Tasks, Workspaces: lf.b.Workspaces,
			LLM: lf.mock, Tools: lf.reg}, Config{Logger: quiet, Now: lf.clock.now}, l)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := lf.job(t, job.ID); got.Status != domain.JobFailed || !strings.Contains(got.ErrorMessage, "more than the learning window") {
			t.Errorf("job = %s %q", got.Status, got.ErrorMessage)
		}
	})
	t.Run("AgentPulse down", func(t *testing.T) {
		lf := newLearningFixture(t, repotest.Memory(), false)
		lf.pulse.FailNext("GET", "/v1/analytics/feedback", http.StatusServiceUnavailable, 1)
		job := lf.queue(t, lf.at)
		lf.run(t)
		if got := lf.job(t, job.ID); got.Status != domain.JobPending || got.Attempts != 1 || got.RetryAfter == nil {
			t.Errorf("job = %s, %d attempts", got.Status, got.Attempts)
		}
	})
	t.Run("learning disabled", func(t *testing.T) {
		lf := newLearningFixture(t, repotest.Memory(), false)
		w, _ := lf.b.Workspaces.Get(ctx, "tenant_acme", "ws")
		if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{DisabledAgents: &[]string{domain.AgentLearning}}, lf.clock.now()); err != nil {
			t.Fatal(err)
		}
		if err := lf.b.Workspaces.Update(ctx, w); err != nil {
			t.Fatal(err)
		}
		job := lf.queue(t, lf.at)
		lf.run(t)
		if got := lf.job(t, job.ID); got.Status != domain.JobFailed || len(lf.pulse.Requests()) != 0 {
			t.Errorf("job = %s, %d AgentPulse calls", got.Status, len(lf.pulse.Requests()))
		}
	})
}

// wideSource answers for 40 days, whatever was asked.
type wideSource struct{}

func (wideSource) FeedbackAnalytics(_ context.Context, _ string, since time.Time) (domain.FeedbackAnalytics, error) {
	return domain.FeedbackAnalytics{Since: since.Add(-10 * 24 * time.Hour), Groups: []domain.FeedbackGroup{
		{EventType: domain.EventTaskUpdated, Delivered: 50, Dismissed: 50, DistinctUsers: 9}}}, nil
}

func (wideSource) Suggestions(context.Context, string) ([]SuggestionState, error) { return nil, nil }
