package domain

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUnblockedAt(t *testing.T) {
	later := t0.Add(time.Hour)
	for name, tc := range map[string]struct {
		to   Status
		want bool
	}{
		"blocked to in_progress": {StatusInProgress, true},
		"blocked to todo":        {StatusTodo, true},
		"blocked to archived":    {StatusArchived, false},
	} {
		t.Run(name, func(t *testing.T) {
			task := taskIn(t, StatusBlocked)
			if task.UnblockedAt != nil {
				t.Fatalf("UnblockedAt before any unblock = %v", task.UnblockedAt)
			}
			if _, err := task.TransitionTo(tc.to, alice, later); err != nil {
				t.Fatal(err)
			}
			if got := task.UnblockedAt != nil && task.UnblockedAt.Equal(later); got != tc.want {
				t.Errorf("UnblockedAt = %v, want set: %v", task.UnblockedAt, tc.want)
			}
		})
	}

	t.Run("dependency release", func(t *testing.T) {
		task, blocker := newTask(t), newTask(t)
		if _, err := task.ApplyDependencyBlock(blocker, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := task.LiftDependencyBlock(blocker, later); err != nil {
			t.Fatal(err)
		}
		if task.UnblockedAt == nil || !task.UnblockedAt.Equal(later) {
			t.Errorf("UnblockedAt = %v", task.UnblockedAt)
		}
	})

	t.Run("other moves leave it alone", func(t *testing.T) {
		task := newTask(t)
		if _, err := task.TransitionTo(StatusInProgress, alice, later); err != nil {
			t.Fatal(err)
		}
		if task.UnblockedAt != nil {
			t.Errorf("UnblockedAt = %v", task.UnblockedAt)
		}
	})
}

func briefTaskFor(t *testing.T, title string, p Priority, s Status, due *time.Time, created time.Time) *Task {
	t.Helper()
	task, _, err := NewTask(NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws_core", Title: title, Priority: p,
		AssigneeID: bob.ID, DueAt: due}, alice, created)
	if err != nil {
		t.Fatal(err)
	}
	if s != StatusTodo {
		if _, err := task.TransitionTo(s, alice, created); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// TestRankBriefTasks pins the ranking of the brief's top items (D39):
// overdue first, then priority, then due date (none last), then started
// work, then the oldest.
func TestRankBriefTasks(t *testing.T) {
	at := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	past, soon, later := at.Add(-time.Hour), at.Add(4*time.Hour), at.Add(72*time.Hour)
	older := at.Add(-48 * time.Hour)
	tasks := []*Task{
		briefTaskFor(t, "low, no due date", PriorityLow, StatusTodo, nil, older),
		briefTaskFor(t, "critical, no due date", PriorityCritical, StatusTodo, nil, older),
		briefTaskFor(t, "high, due later", PriorityHigh, StatusTodo, &later, older),
		briefTaskFor(t, "high, due soon", PriorityHigh, StatusTodo, &soon, older),
		briefTaskFor(t, "low, overdue", PriorityLow, StatusTodo, &past, older),
		briefTaskFor(t, "high, no due date, started", PriorityHigh, StatusInProgress, nil, at.Add(-time.Hour)),
		briefTaskFor(t, "high, no due date, newer", PriorityHigh, StatusTodo, nil, at.Add(-time.Hour)),
		briefTaskFor(t, "high, no due date, older", PriorityHigh, StatusTodo, nil, older),
		briefTaskFor(t, "medium, overdue", PriorityMedium, StatusInProgress, &past, older),
	}
	want := []string{
		"medium, overdue", "low, overdue", // overdue first, then by priority
		"critical, no due date",
		"high, due soon", "high, due later", // earliest due date
		"high, no due date, started", // started before not started
		"high, no due date, older", "high, no due date, newer",
		"low, no due date",
	}
	var got []string
	for _, task := range RankBriefTasks(tasks, at) {
		got = append(got, task.Title)
	}
	if !slices.Equal(got, want) {
		t.Errorf("rank =\n%v\nwant\n%v", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	reversed := slices.Clone(tasks)
	slices.Reverse(reversed)
	if again := RankBriefTasks(reversed, at); again[0].Title != want[0] || again[len(again)-1].Title != want[len(want)-1] {
		t.Error("the ranking depends on the input order")
	}
	if tasks[0].Title != "low, no due date" {
		t.Error("RankBriefTasks reordered its input")
	}
}

func briefJob(t *testing.T, user string, at time.Time) (*AgentJob, BriefInput) {
	t.Helper()
	job, err := NewBriefJob(BriefRecipient{TenantID: "tenant_acme", UserID: user, WorkspaceID: "ws_core"}, at, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseBriefInput(job.InputContext)
	if err != nil {
		t.Fatal(err)
	}
	return &job, in
}

func TestBriefJob(t *testing.T) {
	at := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	job, in := briefJob(t, "usr_bob", at)
	if job.TaskID != nil || job.AgentType != AgentDigest || job.TenantID != "tenant_acme" || job.Status != JobPending ||
		job.DedupKey != "digest:tenant_acme:usr_bob:brief:2026-11-22" || job.BaseTaskVersion != nil {
		t.Errorf("job = %+v", job)
	}
	if in.Trigger != TriggerMorningBrief || in.UserID != "usr_bob" || in.WorkspaceID != "ws_core" || in.Date != "2026-11-22" ||
		!in.At.Equal(at) || !in.Since().Equal(at.Add(-24*time.Hour)) {
		t.Errorf("input = %+v", in)
	}
	// The runtime finds the job's workspace in its input context (D23).
	var scope struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(job.InputContext, &scope); err != nil || scope.WorkspaceID != "ws_core" {
		t.Errorf("input context %s does not name the workspace", job.InputContext)
	}
	if again, _ := briefJob(t, "usr_bob", at.Add(10*time.Hour)); again.DedupKey != job.DedupKey {
		t.Error("two briefs on one day have different keys")
	}
	if next, _ := briefJob(t, "usr_bob", at.Add(24*time.Hour)); next.DedupKey == job.DedupKey {
		t.Error("the next day's brief has the same key")
	}
	if _, err := NewBriefJob(BriefRecipient{TenantID: "tenant_acme", WorkspaceID: "ws_core"}, at, at); err == nil {
		t.Error("a brief without a user was accepted")
	}
	if _, err := ParseBriefInput(json.RawMessage(`{"trigger": "task.updated"}`)); err == nil {
		t.Error("a non-brief input was accepted")
	}
}

func TestMorningBrief(t *testing.T) {
	at := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	job, in := briefJob(t, "usr_bob", at)
	past, created := at.Add(-time.Hour), at.Add(-72*time.Hour)
	overdue := briefTaskFor(t, "Overdue report", PriorityLow, StatusTodo, &past, created)
	critical := briefTaskFor(t, "Outage", PriorityCritical, StatusInProgress, nil, created)
	high := briefTaskFor(t, "Review", PriorityHigh, StatusTodo, nil, created)
	medium := briefTaskFor(t, "Docs", PriorityMedium, StatusTodo, nil, created)
	// Unblocked within the window, and before it.
	fresh := taskIn(t, StatusBlocked)
	fresh.Title = "Freshly unblocked"
	if _, err := fresh.TransitionTo(StatusInProgress, alice, at.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale := taskIn(t, StatusBlocked)
	if _, err := stale.TransitionTo(StatusTodo, alice, at.Add(-30*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale.Priority = PriorityLow

	approval := func(asked time.Time, tools string) BriefApproval {
		j := AgentJob{ID: uuid.New(), AgentType: AgentTriage, UpdatedAt: asked, ProposedActions: json.RawMessage(tools)}
		return NewBriefApproval(&j, high)
	}
	var approvals []BriefApproval
	for i := range BriefListMax + 2 {
		approvals = append(approvals, approval(at.Add(-time.Duration(i)*time.Minute), `[{"tool": "UpdateTaskAttributes"}]`))
	}

	b := NewMorningBrief(job, in, []*Task{medium, high, fresh, critical, overdue, stale}, approvals)
	var top []string
	for _, item := range b.TopItems {
		top = append(top, item.Title)
	}
	if !slices.Equal(top, []string{"Overdue report", "Outage", "Review"}) || !b.TopItems[0].Overdue || b.TopItems[1].Overdue {
		t.Errorf("top items = %v", b.TopItems)
	}
	if b.OpenTasks != 6 || b.Empty() {
		t.Errorf("open tasks = %d, empty %v", b.OpenTasks, b.Empty())
	}
	if b.UnblockedTotal != 1 || len(b.UnblockedTasks) != 1 || b.UnblockedTasks[0].ID != fresh.ID {
		t.Errorf("unblocked = %v (total %d)", b.UnblockedTasks, b.UnblockedTotal)
	}
	if b.PendingApprovalsTotal != BriefListMax+2 || len(b.PendingApprovals) != BriefListMax ||
		!b.PendingApprovals[0].AskedAt.Before(b.PendingApprovals[1].AskedAt) ||
		!slices.Equal(b.PendingApprovals[0].Tools, []string{"UpdateTaskAttributes"}) || b.PendingApprovals[0].TaskTitle != "Review" {
		t.Errorf("approvals = %+v (total %d)", b.PendingApprovals, b.PendingApprovalsTotal)
	}

	t.Run("nothing to report", func(t *testing.T) {
		empty := NewMorningBrief(job, in, nil, nil)
		if !empty.Empty() || empty.TopItems == nil || empty.PendingApprovals == nil || empty.UnblockedTasks == nil {
			t.Errorf("empty brief = %+v", empty)
		}
		if NewMorningBrief(job, in, nil, approvals[:1]).Empty() {
			t.Error("a brief with a pending approval is empty")
		}
	})

	t.Run("notification", func(t *testing.T) {
		b.Text = "Start with the overdue report."
		n := b.Notification(baseURL)
		if n.IdempotencyKey != "taskify:tenant_acme:"+job.ID.String()+":digest.morning_brief" || n.UserID != "usr_bob" ||
			n.TenantID != "tenant_acme" || n.EventType != EventDigestMorningBrief || n.Source != "taskify" {
			t.Errorf("envelope = %+v", n)
		}
		if !slices.Equal(n.Channels, []string{"digest"}) || n.Payload["digestSummary"] != b.Text || n.Body != b.Text ||
			n.Title != "Your morning brief for 2026-11-22" || n.Payload["bypassDigest"] != nil {
			t.Errorf("routing: channels %v payload %v", n.Channels, n.Payload)
		}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		p := wire.Payload
		items := p["topItems"].([]any)
		first := items[0].(map[string]any)
		if len(items) != 3 || first["taskId"] != overdue.ID.String() || first["overdue"] != true ||
			first["dueAt"] != past.Format(time.RFC3339) {
			t.Errorf("topItems = %v", items)
		}
		if p["openTasks"] != float64(6) || p["pendingApprovalsTotal"] != float64(BriefListMax+2) ||
			len(p["pendingApprovals"].([]any)) != BriefListMax || len(p["unblockedTasks"].([]any)) != 1 {
			t.Errorf("payload = %v", p)
		}
		if urls := p["actionUrls"].(map[string]any); urls["view"] != baseURL+"/api/v1/me/brief" {
			t.Errorf("actionUrls = %v", urls)
		}
		var keys []string
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, child := range x {
					keys = append(keys, k)
					walk(child)
				}
			case []any:
				for _, child := range x {
					walk(child)
				}
			}
		}
		walk(p)
		for _, k := range keys {
			if strings.Contains(k, "_") {
				t.Errorf("payload key %q is not camelCase", k)
			}
		}
	})

	t.Run("report JSON", func(t *testing.T) {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"brief_date", "user_id", "top_items", "open_tasks", "pending_approvals",
			"pending_approvals_total", "unblocked_tasks", "unblocked_total", "text"} {
			if _, ok := back[k]; !ok {
				t.Errorf("report is missing %q: %s", k, raw)
			}
		}
		if _, leaked := back["TenantID"]; leaked {
			t.Errorf("report carries the tenant: %s", raw)
		}
	})
}
