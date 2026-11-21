package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func titles(plan []PlannedSubtask) []string {
	out := make([]string, len(plan))
	for i, p := range plan {
		out[i] = p.Title
	}
	return out
}

func TestPlanSubtasksOrdersByDependency(t *testing.T) {
	// Given out of order: Deploy needs Test, which needs Build.
	plan, err := PlanSubtasks([]SubtaskSpec{
		{Title: "Deploy", DependsOn: []string{"test", "Build"}},
		{Title: " Test ", DependsOn: []string{"BUILD", "Build"}}, // case and repeats do not matter
		{Title: "Build"},
		{Title: "Write docs"},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(plan); !slices.Equal(got, []string{"Build", "Test", "Deploy", "Write docs"}) {
		t.Fatalf("order = %v", got)
	}
	want := [][]int{nil, {0}, {1, 0}, nil}
	for i, p := range plan {
		if !slices.Equal(p.Blockers, want[i]) {
			t.Errorf("%s blockers = %v, want %v", p.Title, p.Blockers, want[i])
		}
		for _, b := range p.Blockers {
			if b >= i {
				t.Errorf("%s comes before its blocker %d", p.Title, b)
			}
		}
	}
}

func TestPlanSubtasksRejects(t *testing.T) {
	long := strings.Repeat("x", 256)
	cases := []struct {
		name  string
		specs []SubtaskSpec
		max   int
		want  string
	}{
		{"none", nil, 5, "at least one"},
		{"over the cap", []SubtaskSpec{{Title: "A"}, {Title: "B"}, {Title: "C"}}, 2, "3 subtasks proposed; this workspace allows at most 2"},
		{"blank title", []SubtaskSpec{{Title: "  "}}, 5, "no title"},
		{"long title", []SubtaskSpec{{Title: long}}, 5, "at most 255"},
		{"long description", []SubtaskSpec{{Title: "A", Description: strings.Repeat("d", 4001)}}, 5, "at most 4000"},
		{"duplicate title", []SubtaskSpec{{Title: "Build"}, {Title: "build "}}, 5, "unique"},
		{"unknown title", []SubtaskSpec{{Title: "A", DependsOn: []string{"Nope"}}}, 5, `"Nope", which is not one of the subtasks`},
		{"self", []SubtaskSpec{{Title: "A", DependsOn: []string{"a"}}}, 5, "cannot depend on itself"},
		{"two-cycle", []SubtaskSpec{{Title: "A", DependsOn: []string{"B"}}, {Title: "B", DependsOn: []string{"A"}}}, 5, "cycle: A, B"},
		{"three-cycle behind a free task", []SubtaskSpec{
			{Title: "Free"},
			{Title: "A", DependsOn: []string{"C", "Free"}}, {Title: "B", DependsOn: []string{"A"}}, {Title: "C", DependsOn: []string{"B"}},
		}, 5, "cycle: A, B, C"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PlanSubtasks(c.specs, c.max)
			if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want validation containing %q", err, c.want)
			}
		})
	}
	// No cap means no limit.
	if _, err := PlanSubtasks([]SubtaskSpec{{Title: "A"}, {Title: "B"}, {Title: "C"}}, 0); err != nil {
		t.Errorf("uncapped: %v", err)
	}
}

func TestNewSubtask(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	parent, _, err := NewTask(NewTaskInput{TenantID: "t", WorkspaceID: "ws", Title: "Launch", Priority: PriorityHigh,
		AssigneeID: "usr_bob", Labels: []string{"launch"}}, User("usr_alice"), now)
	if err != nil {
		t.Fatal(err)
	}
	agent := Agent(AgentDecomposer)

	free, events, err := NewSubtask(parent, PlannedSubtask{SubtaskSpec: SubtaskSpec{Title: "Build", Description: "d"}}, "usr_carol", agent, now)
	if err != nil {
		t.Fatal(err)
	}
	if free.ParentID == nil || *free.ParentID != parent.ID || free.WorkspaceID != "ws" || free.Priority != PriorityHigh ||
		free.AssigneeID != "usr_bob" || free.CreatorID != "usr_carol" || free.Status != StatusTodo || free.Version != 1 {
		t.Errorf("subtask = %+v", free)
	}
	if len(events) != 1 || events[0].Type != EventTaskCreated || events[0].Actor != agent ||
		!slices.Equal(events[0].Recipients(), []string{"usr_bob"}) {
		t.Errorf("events = %+v", events)
	}

	blocked, events, err := NewSubtask(parent, PlannedSubtask{SubtaskSpec: SubtaskSpec{Title: "Test"}, Blockers: []int{0}}, "usr_carol", agent, now)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status != StatusBlocked || *blocked.BlockedReason != BlockedDependency || *blocked.StatusBeforeBlocked != StatusTodo {
		t.Errorf("a subtask with blockers = %s %v %v", blocked.Status, blocked.BlockedReason, blocked.StatusBeforeBlocked)
	}
	if len(events) != 0 {
		t.Errorf("a blocked subtask raised %+v", events)
	}
	if _, _, err := NewSubtask(parent, PlannedSubtask{SubtaskSpec: SubtaskSpec{Title: "X"}}, "", agent, now); !errors.Is(err, ErrValidation) {
		t.Errorf("no creator: %v", err)
	}
}

func TestSubtaskRecords(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	job := uuid.New()
	var task Task
	task.RecordAgentRun(AgentRunRecord{JobID: uuid.New(), Agent: AgentTriage})
	if _, ok := task.RunOf(job); ok {
		t.Fatal("found a run for another job")
	}
	task.RecordAgentRun(AgentRunRecord{JobID: job, Agent: AgentDecomposer,
		Changes: map[string]any{"subtask_ids": []string{a.String(), b.String()}}})
	r, ok := task.RunOf(job)
	if !ok || !slices.Equal(r.SubtaskIDs(), []uuid.UUID{a, b}) {
		t.Fatalf("run = %+v", r)
	}
	// As read back from JSON.
	r.Changes["subtask_ids"] = []any{a.String(), 7, "not-a-uuid"}
	if !slices.Equal(r.SubtaskIDs(), []uuid.UUID{a}) {
		t.Errorf("ids from JSON = %v", r.SubtaskIDs())
	}
}
