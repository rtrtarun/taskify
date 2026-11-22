package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	t0    = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	alice = User("usr_alice")
	bob   = User("usr_bob")
)

func ptr[T any](v T) *T { return &v }

// newTask returns a task created by alice and assigned to bob.
func newTask(t *testing.T) *Task {
	t.Helper()
	task, _, err := NewTask(NewTaskInput{
		TenantID:    "tenant_acme",
		WorkspaceID: "ws_core",
		Title:       "Fix checkout crash",
		AssigneeID:  bob.ID,
		Watchers:    []string{"usr_carol"},
	}, alice, t0)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	return task
}

// taskIn returns a task driven to the given status through legal moves.
func taskIn(t *testing.T, s Status) *Task {
	t.Helper()
	task := newTask(t)
	path := map[Status][]Status{
		StatusTodo:       nil,
		StatusInProgress: {StatusInProgress},
		StatusBlocked:    {StatusInProgress, StatusBlocked},
		StatusCompleted:  {StatusCompleted},
		StatusArchived:   {StatusArchived},
	}[s]
	for _, step := range path {
		if _, err := task.TransitionTo(step, alice, t0); err != nil {
			t.Fatalf("setup move to %s: %v", step, err)
		}
	}
	return task
}

func eventTypes(events []Event) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func TestNewTask(t *testing.T) {
	task, events, err := NewTask(NewTaskInput{
		TenantID:    "tenant_acme",
		WorkspaceID: "ws_core",
		Title:       "  Fix checkout crash  ",
		AssigneeID:  bob.ID,
		Labels:      []string{"bug", " bug ", "", "backend"},
	}, alice, t0)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	if task.Status != StatusTodo || task.Priority != PriorityMedium || task.Version != 1 {
		t.Errorf("defaults: status %s priority %s version %d", task.Status, task.Priority, task.Version)
	}
	if task.Title != "Fix checkout crash" || task.TenantID != "tenant_acme" {
		t.Errorf("input not trimmed: %q %q", task.Title, task.TenantID)
	}
	if task.CreatorID != alice.ID {
		t.Errorf("CreatorID = %q", task.CreatorID)
	}
	if !slices.Equal(task.Labels, []string{"bug", "backend"}) {
		t.Errorf("Labels = %v", task.Labels)
	}
	if task.ID.Version() != 7 {
		t.Errorf("ID version = %d, want UUIDv7", task.ID.Version())
	}
	if task.Metadata == nil || task.Watchers == nil {
		t.Error("Metadata and Watchers must not be nil")
	}
	if got := eventTypes(events); !slices.Equal(got, []string{EventTaskCreated}) {
		t.Errorf("events = %v", got)
	}
}

func TestNewTaskEvents(t *testing.T) {
	cases := []struct {
		name     string
		assignee string
		want     []string
	}{
		{"assigned to someone else", bob.ID, []string{EventTaskCreated}},
		{"assigned to the creator", alice.ID, []string{}},
		{"unassigned", "", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, events, err := NewTask(NewTaskInput{TenantID: "t", WorkspaceID: "w", Title: "x", AssigneeID: tc.assignee}, alice, t0)
			if err != nil {
				t.Fatal(err)
			}
			if got := eventTypes(events); !slices.Equal(got, tc.want) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewTaskValidation(t *testing.T) {
	base := func() NewTaskInput {
		return NewTaskInput{TenantID: "t", WorkspaceID: "w", Title: "x"}
	}
	manyLabels := make([]string, maxLabels+1)
	for i := range manyLabels {
		manyLabels[i] = "l" + strings.Repeat("x", i+1)
	}
	cases := []struct {
		name   string
		mutate func(*NewTaskInput)
		actor  Actor
	}{
		{"missing tenant", func(in *NewTaskInput) { in.TenantID = " " }, alice},
		{"long tenant", func(in *NewTaskInput) { in.TenantID = strings.Repeat("t", 65) }, alice},
		{"padded tenant", func(in *NewTaskInput) { in.TenantID = " tenant_acme " }, alice},
		{"reserved tenant", func(in *NewTaskInput) { in.TenantID = "default" }, alice},
		{"tenant with a space", func(in *NewTaskInput) { in.TenantID = "tenant acme" }, alice},
		{"missing workspace", func(in *NewTaskInput) { in.WorkspaceID = "" }, alice},
		{"long workspace", func(in *NewTaskInput) { in.WorkspaceID = strings.Repeat("w", 65) }, alice},
		{"missing title", func(in *NewTaskInput) { in.Title = "   " }, alice},
		{"long title", func(in *NewTaskInput) { in.Title = strings.Repeat("x", 256) }, alice},
		{"unknown priority", func(in *NewTaskInput) { in.Priority = "p0" }, alice},
		{"missing creator", func(in *NewTaskInput) {}, User("")},
		{"long creator", func(in *NewTaskInput) {}, User(strings.Repeat("u", 129))},
		{"long assignee", func(in *NewTaskInput) { in.AssigneeID = strings.Repeat("u", 129) }, alice},
		{"too many labels", func(in *NewTaskInput) { in.Labels = manyLabels }, alice},
		{"long label", func(in *NewTaskInput) { in.Labels = []string{strings.Repeat("l", 65)} }, alice},
		{"long watcher", func(in *NewTaskInput) { in.Watchers = []string{strings.Repeat("u", 129)} }, alice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			_, _, err := NewTask(in, tc.actor, t0)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
		})
	}

	t.Run("a 255-character multibyte title is accepted", func(t *testing.T) {
		in := base()
		in.Title = strings.Repeat("é", 255)
		if _, _, err := NewTask(in, alice, t0); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestTransitionMatrix covers all 25 cells of the matrix in
// docs/architecture.md section 4.3.
func TestTransitionMatrix(t *testing.T) {
	allowed := map[Status][]Status{
		StatusTodo:       {StatusInProgress, StatusCompleted, StatusBlocked, StatusArchived},
		StatusInProgress: {StatusBlocked, StatusCompleted, StatusTodo, StatusArchived},
		StatusBlocked:    {StatusInProgress, StatusTodo, StatusArchived},
		StatusCompleted:  {StatusInProgress, StatusArchived},
		StatusArchived:   {StatusTodo},
	}
	for _, from := range Statuses {
		for _, to := range Statuses {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				task := taskIn(t, from)
				before := task.clone()
				want := slices.Contains(allowed[from], to)

				if got := task.CanTransition(to); got != want {
					t.Errorf("CanTransition = %v, want %v", got, want)
				}
				_, err := task.TransitionTo(to, alice, t0.Add(time.Hour))

				switch {
				case from == to:
					// Not a transition: a no-op, with nothing changed.
					if err != nil {
						t.Errorf("same-status move returned %v", err)
					}
					if task.Version != before.Version {
						t.Errorf("version changed on a no-op")
					}
				case want:
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if task.Status != to || task.Version != before.Version+1 {
						t.Errorf("status %s version %d", task.Status, task.Version)
					}
				default:
					if !errors.Is(err, ErrInvalidStateTransition) {
						t.Fatalf("error = %v, want ErrInvalidStateTransition", err)
					}
					if task.Status != from || task.Version != before.Version || !task.UpdatedAt.Equal(before.UpdatedAt) {
						t.Errorf("task changed by a rejected transition")
					}
				}
			})
		}
	}
}

func TestTransitionSideEffects(t *testing.T) {
	later := t0.Add(2 * time.Hour)

	t.Run("completing sets completed_at; reopening clears it", func(t *testing.T) {
		task := taskIn(t, StatusInProgress)
		if _, err := task.TransitionTo(StatusCompleted, alice, later); err != nil {
			t.Fatal(err)
		}
		if task.CompletedAt == nil || !task.CompletedAt.Equal(later) {
			t.Fatalf("CompletedAt = %v", task.CompletedAt)
		}
		if _, err := task.TransitionTo(StatusInProgress, alice, later); err != nil {
			t.Fatal(err)
		}
		if task.CompletedAt != nil {
			t.Errorf("CompletedAt not cleared on reopen")
		}
	})

	t.Run("todo completes directly with one event", func(t *testing.T) {
		task := newTask(t)
		events, err := task.TransitionTo(StatusCompleted, bob, later)
		if err != nil {
			t.Fatal(err)
		}
		if got := eventTypes(events); !slices.Equal(got, []string{EventTaskCompleted}) {
			t.Errorf("events = %v", got)
		}
		if events[0].PreviousStatus != StatusTodo {
			t.Errorf("PreviousStatus = %s", events[0].PreviousStatus)
		}
	})

	t.Run("archiving sets archived_at; restoring clears it", func(t *testing.T) {
		task := taskIn(t, StatusCompleted)
		if _, err := task.TransitionTo(StatusArchived, alice, later); err != nil {
			t.Fatal(err)
		}
		if task.ArchivedAt == nil || task.CompletedAt == nil {
			t.Fatalf("ArchivedAt %v CompletedAt %v: archive keeps completed_at", task.ArchivedAt, task.CompletedAt)
		}
		if _, err := task.TransitionTo(StatusTodo, alice, later); err != nil {
			t.Fatal(err)
		}
		if task.ArchivedAt != nil || task.CompletedAt != nil {
			t.Errorf("restore left ArchivedAt %v CompletedAt %v", task.ArchivedAt, task.CompletedAt)
		}
	})

	t.Run("blocking records where the task came from and why", func(t *testing.T) {
		for _, from := range []Status{StatusTodo, StatusInProgress} {
			task := taskIn(t, from)
			events, err := task.TransitionTo(StatusBlocked, alice, later)
			if err != nil {
				t.Fatal(err)
			}
			if task.StatusBeforeBlocked == nil || *task.StatusBeforeBlocked != from {
				t.Errorf("from %s: StatusBeforeBlocked = %v", from, task.StatusBeforeBlocked)
			}
			if task.BlockedReason == nil || *task.BlockedReason != BlockedManual {
				t.Errorf("from %s: BlockedReason = %v, want manual", from, task.BlockedReason)
			}
			if got := eventTypes(events); !slices.Equal(got, []string{EventTaskBlocked}) {
				t.Errorf("events = %v", got)
			}
		}
	})

	t.Run("leaving blocked clears both columns", func(t *testing.T) {
		for _, to := range []Status{StatusInProgress, StatusTodo, StatusArchived} {
			task := taskIn(t, StatusBlocked)
			if _, err := task.TransitionTo(to, alice, later); err != nil {
				t.Fatal(err)
			}
			if task.StatusBeforeBlocked != nil || task.BlockedReason != nil {
				t.Errorf("to %s: block columns not cleared", to)
			}
		}
	})

	t.Run("a dependency block can be requested", func(t *testing.T) {
		task := newTask(t)
		_, err := task.Apply(Patch{Status: ptr(StatusBlocked), BlockedReason: BlockedDependency}, System, later)
		if err != nil {
			t.Fatal(err)
		}
		if *task.BlockedReason != BlockedDependency {
			t.Errorf("BlockedReason = %v", *task.BlockedReason)
		}
	})

	t.Run("an unknown blocked reason is rejected", func(t *testing.T) {
		task := newTask(t)
		_, err := task.Apply(Patch{Status: ptr(StatusBlocked), BlockedReason: "tired"}, alice, later)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("error = %v, want ErrValidation", err)
		}
		if task.Status != StatusTodo {
			t.Errorf("task changed by a rejected patch")
		}
	})

	t.Run("an unknown status is a validation error", func(t *testing.T) {
		task := newTask(t)
		if _, err := task.TransitionTo("done", alice, later); !errors.Is(err, ErrValidation) {
			t.Fatalf("error = %v, want ErrValidation", err)
		}
	})
}

func TestApplyEvents(t *testing.T) {
	due := t0.Add(48 * time.Hour)
	cases := []struct {
		name  string
		patch Patch
		want  []string
	}{
		{"title", Patch{Title: ptr("New title")}, []string{EventTaskUpdated}},
		{"description", Patch{Description: ptr("More detail")}, []string{EventTaskUpdated}},
		{"due date set", Patch{DueAt: SetTime(due)}, []string{EventTaskUpdated}},
		{"reassigned", Patch{AssigneeID: ptr("usr_dave")}, []string{EventTaskAssigned}},
		{"unassigned", Patch{AssigneeID: ptr("")}, []string{}},
		{"priority raised", Patch{Priority: ptr(PriorityCritical)}, []string{EventTaskPriorityEscalated}},
		{"priority lowered", Patch{Priority: ptr(PriorityLow)}, []string{}},
		{"labels", Patch{Labels: ptr([]string{"bug"})}, []string{}},
		{"watchers", Patch{Watchers: ptr([]string{"usr_erin"})}, []string{}},
		{"reminder", Patch{RemindAt: SetTime(due)}, []string{}},
		{"metadata", Patch{Metadata: ptr(map[string]any{"k": "v"})}, []string{}},
		{"started", Patch{Status: ptr(StatusInProgress)}, []string{}},
		{"blocked", Patch{Status: ptr(StatusBlocked)}, []string{EventTaskBlocked}},
		{"completed", Patch{Status: ptr(StatusCompleted)}, []string{EventTaskCompleted}},
		{"archived", Patch{Status: ptr(StatusArchived)}, []string{}},
		{
			"several changes at once",
			Patch{Title: ptr("New"), AssigneeID: ptr("usr_dave"), Priority: ptr(PriorityHigh), Status: ptr(StatusBlocked)},
			[]string{EventTaskAssigned, EventTaskPriorityEscalated, EventTaskBlocked, EventTaskUpdated},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := newTask(t)
			events, err := task.Apply(tc.patch, alice, t0.Add(time.Minute))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := eventTypes(events); !slices.Equal(got, tc.want) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
			if task.Version != 2 {
				t.Errorf("Version = %d, want 2", task.Version)
			}
			if !task.UpdatedAt.Equal(t0.Add(time.Minute)) {
				t.Errorf("UpdatedAt = %v", task.UpdatedAt)
			}
			for _, e := range events {
				if e.Task.Version != 2 {
					t.Errorf("%s carries version %d, want 2", e.Type, e.Task.Version)
				}
			}
		})
	}
}

func TestApplyNoChange(t *testing.T) {
	task := newTask(t)
	events, err := task.Apply(Patch{Title: ptr("Fix checkout crash"), Priority: ptr(PriorityMedium), Status: ptr(StatusTodo)}, alice, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || task.Version != 1 || !task.UpdatedAt.Equal(t0) {
		t.Errorf("no-op patch changed the task: events %v version %d", events, task.Version)
	}
}

func TestApplyRejectedPatchLeavesTaskUntouched(t *testing.T) {
	task := newTask(t)
	before := task.clone()
	// A valid title change combined with an invalid priority.
	_, err := task.Apply(Patch{Title: ptr("Changed"), Priority: ptr(Priority("urgent"))}, alice, t0.Add(time.Hour))
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if task.Title != before.Title || task.Version != before.Version {
		t.Errorf("task changed by a rejected patch")
	}
}

func TestApplyDueDateResetsReminder(t *testing.T) {
	task := newTask(t)
	task.LastDueSoonNotifiedAt = &t0
	task.LastOverdueNotifiedAt = &t0

	if _, err := task.Apply(Patch{DueAt: SetTime(t0.Add(72 * time.Hour))}, alice, t0); err != nil {
		t.Fatal(err)
	}
	if task.LastDueSoonNotifiedAt != nil {
		t.Error("LastDueSoonNotifiedAt not cleared when due_at changed")
	}

	task.LastDueSoonNotifiedAt = &t0
	if _, err := task.Apply(Patch{Title: ptr("Other")}, alice, t0); err != nil {
		t.Fatal(err)
	}
	if task.LastDueSoonNotifiedAt == nil {
		t.Error("LastDueSoonNotifiedAt cleared by an unrelated change")
	}

	if _, err := task.Apply(Patch{DueAt: ClearTime()}, alice, t0); err != nil {
		t.Fatal(err)
	}
	if task.DueAt != nil {
		t.Error("DueAt not cleared")
	}
}

func TestEventSnapshotIsIndependent(t *testing.T) {
	task := newTask(t)
	events, err := task.Apply(Patch{Title: ptr("Snapshot")}, alice, t0)
	if err != nil {
		t.Fatal(err)
	}
	task.Labels = append(task.Labels, "later")
	task.Watchers[0] = "changed"
	if len(events[0].Task.Labels) != 0 || events[0].Task.Watchers[0] != "usr_carol" {
		t.Errorf("event snapshot shares state with the task")
	}
}

func TestPriorityHelpers(t *testing.T) {
	if !PriorityCritical.HigherThan(PriorityHigh) || PriorityLow.HigherThan(PriorityMedium) || PriorityHigh.HigherThan(PriorityHigh) {
		t.Error("HigherThan ordering is wrong")
	}
	if Priority("x").HigherThan(PriorityLow) || PriorityLow.HigherThan("x") {
		t.Error("an unknown priority must not compare as higher")
	}
	wantHints := map[Priority]string{PriorityCritical: "critical", PriorityHigh: "high", PriorityMedium: "medium", PriorityLow: "low"}
	for p, want := range wantHints {
		if p.Hint() != want {
			t.Errorf("%s.Hint() = %q, want %q", p, p.Hint(), want)
		}
		if p.Label() == "" || !p.Valid() {
			t.Errorf("%s: label or validity wrong", p)
		}
	}
	if Status("x").Valid() || BlockedReason("x").Valid() {
		t.Error("unknown values reported as valid")
	}
}

func TestIsOpen(t *testing.T) {
	for _, s := range Statuses {
		want := s != StatusCompleted && s != StatusArchived
		if got := taskIn(t, s).IsOpen(); got != want {
			t.Errorf("%s: IsOpen = %v, want %v", s, got, want)
		}
	}
}
