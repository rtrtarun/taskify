package domain_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/contract"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

// TestEveryNotificationPassesAgentPulseActionChecks is AP-02's exit check:
// every kind of notification Taskify builds is accepted by the mock, which
// enforces AgentPulse's 40b rules (only the six action keys, each with the ID
// its callback needs), and uses no key outside the six. It runs without
// APP_BASE_URL and with it, whose view links must pass the same checks
// (AP-10).
func TestEveryNotificationPassesAgentPulseActionChecks(t *testing.T) {
	t.Run("without the app", func(t *testing.T) {
		checkEveryNotification(t, domain.Links{API: "https://taskify.test"})
	})
	t.Run("with the app", func(t *testing.T) {
		checkEveryNotification(t, domain.Links{API: "https://taskify.test", App: "https://app.taskify.test"})
	})
}

func checkEveryNotification(t *testing.T, links domain.Links) {
	const tenant = "tenant_acme"
	baseURL := links.API
	var (
		alice = domain.User("usr_alice")
		bob   = "usr_bob"
		t0    = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
		due   = t0.Add(30 * time.Hour)
	)
	newTask := func(t *testing.T, title string, p domain.Priority) *domain.Task {
		t.Helper()
		task, _, err := domain.NewTask(domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: title,
			Priority: p, AssigneeID: bob, Watchers: []string{"usr_dave"}, DueAt: &due}, alice, t0)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	apply := func(t *testing.T, task *domain.Task, p domain.Patch) []domain.Event {
		t.Helper()
		events, err := task.Apply(p, alice, t0.Add(time.Minute))
		if err != nil || len(events) == 0 {
			t.Fatalf("Apply: %v, %v", events, err)
		}
		return events
	}
	status := func(s domain.Status) *domain.Status { return &s }
	str := func(s string) *string { return &s }
	prio := func(p domain.Priority) *domain.Priority { return &p }

	var notes []domain.Notification
	addEvents := func(events ...domain.Event) {
		for _, e := range events {
			notes = append(notes, e.Notifications(links)...)
		}
	}

	// Creation, edits, and status changes.
	created, events, err := domain.NewTask(domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "Created",
		AssigneeID: bob}, alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	addEvents(events...)
	addEvents(apply(t, newTask(t, "Reassigned", domain.PriorityMedium), domain.Patch{AssigneeID: str("usr_carol")})...)
	addEvents(apply(t, newTask(t, "Edited", domain.PriorityMedium), domain.Patch{Title: str("Edited again")})...)
	addEvents(apply(t, newTask(t, "Raised", domain.PriorityLow), domain.Patch{Priority: prio(domain.PriorityCritical)})...)
	addEvents(apply(t, newTask(t, "Blocked by hand", domain.PriorityMedium), domain.Patch{Status: status(domain.StatusBlocked)})...)
	addEvents(apply(t, newTask(t, "Done", domain.PriorityMedium), domain.Patch{Status: status(domain.StatusCompleted)})...)
	reminded := newTask(t, "Reminded", domain.PriorityHigh)
	addEvents(domain.NewDueSoonEvent(reminded, domain.DueWindow24h, t0), domain.NewDueSoonEvent(reminded, domain.DueWindow1h, t0),
		domain.NewOverdueEvent(reminded, due.Add(time.Hour)))
	commented := newTask(t, "Commented", domain.PriorityMedium)
	_, events, err = domain.NewComment(commented, "Looks good @usr_erin", alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	addEvents(events...)

	// A deferred assignment: digest only.
	deferred := apply(t, newTask(t, "Deferred", domain.PriorityMedium), domain.Patch{AssigneeID: str("usr_frank")})
	deferred[0].Deferred = true
	addEvents(deferred...)

	// Dependencies.
	blocker := newTask(t, "Blocker", domain.PriorityMedium)
	dependent := newTask(t, "Dependent", domain.PriorityMedium)
	events, err = dependent.ApplyDependencyBlock(blocker, t0)
	if err != nil {
		t.Fatal(err)
	}
	addEvents(events...)
	events, err = dependent.LiftDependencyBlock(blocker, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	addEvents(events...)
	for _, typ := range []string{domain.EventTaskDependencyResolved, domain.EventTaskDependencyAdded, domain.EventTaskDependencyReopened} {
		addEvents(domain.NewDependencyEvent(typ, newTask(t, typ, domain.PriorityMedium), blocker, t0))
	}
	finished := newTask(t, "Finished dependent", domain.PriorityMedium)
	apply(t, finished, domain.Patch{Status: status(domain.StatusCompleted)})
	addEvents(domain.NewDependencyEvent(domain.EventTaskDependencyReopened, finished, blocker, t0))

	// Agents: an approval request, escalations with and without unblock, and
	// a morning brief.
	proposed := newTask(t, "Proposed", domain.PriorityMedium)
	job, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, TaskID: &proposed.ID, AgentType: domain.AgentTriage,
		DedupKey: "triage:" + proposed.ID.String()}, t0)
	if err != nil {
		t.Fatal(err)
	}
	addEvents(domain.NewApprovalRequestedEvent(proposed, &job, []string{"UpdateTaskAttributes"}, t0))
	stuck := newTask(t, "Stuck", domain.PriorityMedium)
	apply(t, stuck, domain.Patch{Status: status(domain.StatusBlocked)})
	stale := newTask(t, "Stale", domain.PriorityMedium)
	if _, err := stale.Apply(domain.Patch{Status: status(domain.StatusInProgress)}, alice, t0); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.NewTask(domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "In the way",
		AssigneeID: "usr_owner"}, alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	investigation, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: tenant, TaskID: &stuck.ID,
		AgentType: domain.AgentBlocker, DedupKey: "blocker:" + stuck.ID.String()}, t0)
	if err != nil {
		t.Fatal(err)
	}
	addEvents(domain.NewEscalationEvents(stuck, &investigation, []*domain.Task{owner}, t0)...)
	addEvents(domain.NewEscalationEvents(stale, &investigation, []*domain.Task{owner}, t0)...)
	brief := domain.MorningBrief{TenantID: tenant, JobID: uuid.New(), Date: "2026-11-22", UserID: bob, Text: "Two things today.",
		TopItems: []domain.BriefTask{{ID: created.ID, Title: "Created", WorkspaceID: "ws_core", Status: domain.StatusTodo,
			Priority: domain.PriorityMedium, DueAt: &due}},
		PendingApprovals: []domain.BriefApproval{{JobID: job.ID, AgentType: domain.AgentTriage, TaskID: proposed.ID,
			TaskTitle: "Proposed", Tools: []string{"UpdateTaskAttributes"}, AskedAt: t0}}, PendingApprovalsTotal: 1}
	notes = append(notes, brief.Notification(links))

	mock := mockagentpulse.New()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	covered := map[string]bool{}
	for _, n := range notes {
		covered[n.EventType] = true
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		// M12-02: the payload matches contract/events/{eventType}.schema.json.
		if err := contract.ValidateEvent(n.EventType, raw); err != nil {
			t.Errorf("%s to %s: %v", n.EventType, n.UserID, err)
		}
		resp, err := http.Post(srv.URL+"/v1/notify", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("%s to %s: status %d %v\n%s", n.EventType, n.UserID, resp.StatusCode, out, raw)
		}
		urls, _ := n.Payload["actionUrls"].(map[string]string)
		for k := range urls {
			if _, ok := mockagentpulse.ActionKeys[k]; !ok {
				t.Errorf("%s uses action key %q", n.EventType, k)
			}
		}
		// Open task notifications offer Mark done.
		if n.EventType == domain.EventTaskCreated && urls[domain.ActionComplete] != baseURL+"/api/v1/tasks/"+created.ID.String()+"/complete" {
			t.Errorf("task.created actionUrls = %v", urls)
		}
		// AP-10: with the app, every notification about a task views it
		// there, and the brief views the brief page. Without it, no
		// notification carries view (D50).
		taskID, _ := n.Payload["taskId"].(string)
		view, hasView := urls[domain.ActionView]
		switch {
		case links.App == "":
			if hasView {
				t.Errorf("%s carries view %q without APP_BASE_URL", n.EventType, view)
			}
		case n.EventType == domain.EventDigestMorningBrief:
			if view != links.App+"/brief" || len(urls) != 1 {
				t.Errorf("brief actionUrls = %v", urls)
			}
		default:
			if view != links.App+"/tasks/"+taskID {
				t.Errorf("%s to %s: view = %q, want the task's page", n.EventType, n.UserID, view)
			}
		}
	}
	for _, typ := range []string{
		domain.EventTaskCreated, domain.EventTaskAssigned, domain.EventTaskUpdated, domain.EventTaskPriorityEscalated,
		domain.EventTaskBlocked, domain.EventTaskDueSoon, domain.EventTaskOverdue, domain.EventTaskCompleted,
		domain.EventTaskCommentAdded, domain.EventTaskUnblocked, domain.EventTaskDependencyResolved,
		domain.EventTaskDependencyAdded, domain.EventTaskDependencyReopened, domain.EventAgentApprovalRequested,
		domain.EventTaskEscalated, domain.EventDigestMorningBrief,
	} {
		if !covered[typ] {
			t.Errorf("no %s notification was checked", typ)
		}
	}
	if accepted := len(mock.Notifications()); accepted != len(notes) || accepted < 20 {
		t.Errorf("accepted %d of %d notifications", accepted, len(notes))
	}
	keys := make([]string, 0, len(mockagentpulse.ActionKeys))
	for k := range mockagentpulse.ActionKeys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{domain.ActionApprove, domain.ActionComplete, domain.ActionReject, domain.ActionSnooze2h, domain.ActionUnblock, domain.ActionView}
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("domain action keys %v differ from the mock's %v", want, keys)
	}
}

// TestFinishedTaskOffersNoActions: "Mark done" and "Snooze 2h" are offered
// only while the task is open.
func TestFinishedTaskOffersNoActions(t *testing.T) {
	task, _, err := domain.NewTask(domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: "x", AssigneeID: "usr_bob"},
		domain.User("usr_alice"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	done := domain.StatusCompleted
	events, err := task.Apply(domain.Patch{Status: &done}, domain.User("usr_bob"), time.Now())
	if err != nil || len(events) != 1 {
		t.Fatalf("complete: %v, %v", events, err)
	}
	for _, n := range events[0].Notifications(domain.Links{API: "https://taskify.test"}) {
		if urls, ok := n.Payload["actionUrls"]; ok {
			t.Errorf("task.completed to %s offers %v", n.UserID, urls)
		}
	}
}

// TestViewLinksOpenTheApp is AP-10: with APP_BASE_URL set, a notification
// about a task views {APP_BASE_URL}/tasks/{id}, whatever its status, next to
// its other actions, and the morning brief views {APP_BASE_URL}/brief.
func TestViewLinksOpenTheApp(t *testing.T) {
	links := domain.Links{API: "https://api.taskify.test", App: "https://app.taskify.test"}
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	task, events, err := domain.NewTask(domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: "x",
		AssigneeID: "usr_bob"}, domain.User("usr_alice"), t0)
	if err != nil || len(events) != 1 {
		t.Fatalf("NewTask: %v, %v", events, err)
	}
	page := "https://app.taskify.test/tasks/" + task.ID.String()
	api := "https://api.taskify.test/api/v1/tasks/" + task.ID.String()
	open := events[0].Notifications(links)[0].Payload["actionUrls"].(map[string]string)
	if open[domain.ActionView] != page || open[domain.ActionComplete] != api+"/complete" || len(open) != 3 {
		t.Errorf("open task actionUrls = %v", open)
	}

	done := domain.StatusCompleted
	events, err = task.Apply(domain.Patch{Status: &done}, domain.User("usr_bob"), t0.Add(time.Hour))
	if err != nil || len(events) != 1 {
		t.Fatalf("complete: %v, %v", events, err)
	}
	for _, n := range events[0].Notifications(links) {
		if urls := n.Payload["actionUrls"].(map[string]string); urls[domain.ActionView] != page || len(urls) != 1 {
			t.Errorf("task.completed to %s: actionUrls = %v, want view only", n.UserID, urls)
		}
	}

	// A trailing slash is trimmed by the configuration, not here: Links
	// joins what it is given.
	if got := (domain.Links{API: "https://api", App: "https://app"}).BriefView(); got != "https://app/brief" {
		t.Errorf("BriefView = %q", got)
	}
	if got := (domain.Links{API: "https://api"}).BriefView(); got != "" {
		t.Errorf("BriefView without the app = %q", got)
	}
	if got := (domain.Links{API: "https://api"}).TaskView(task.ID); got != "" {
		t.Errorf("TaskView without the app = %q", got)
	}
}
