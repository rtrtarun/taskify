package service

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

var decomposer = domain.Agent(domain.AgentDecomposer)

// launchPlan is a parent split into Build, then Test, then Release, which
// waits for both; Docs is independent.
var launchPlan = []domain.SubtaskSpec{
	{Title: "Release", DependsOn: []string{"Build", "Test"}},
	{Title: "Test", DependsOn: []string{"Build"}},
	{Title: "Build", Description: "Compile and package."},
	{Title: "Docs"},
}

// taskCount returns how many tasks the tenant has, archived ones included.
func (f *fixture) taskCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, s := range []domain.Status{"", domain.StatusArchived} {
		page, err := f.b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant, Status: s, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		n += len(page.Tasks)
	}
	return n
}

func (f *fixture) decompose(t *testing.T, parent *domain.Task, job uuid.UUID, specs []domain.SubtaskSpec) domain.SubtaskResult {
	t.Helper()
	cur := f.get(t, parent.ID)
	res, err := f.svc.CreateSubtasks(ctx, decomposer, tenant, parent.ID, cur.Version, alice.ID, specs,
		domain.AgentRunRecord{JobID: job, Agent: domain.AgentDecomposer, Reasoning: "Split by stage."})
	if err != nil {
		t.Fatalf("CreateSubtasks: %v", err)
	}
	return res
}

func byTitle(res domain.SubtaskResult) map[string]*domain.Task {
	out := map[string]*domain.Task{}
	for _, c := range res.Subtasks {
		out[c.Title] = c
	}
	return out
}

func TestCreateSubtasksBuildsTheDAG(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t) // so a wrongly attributed child would queue triage
		parent := f.create(t, func(in *domain.NewTaskInput) { in.Priority = domain.PriorityHigh })
		job := uuid.New()

		res := f.decompose(t, parent, job, launchPlan)
		if !res.Created || len(res.Subtasks) != 4 {
			t.Fatalf("result = %+v", res)
		}
		var order []string
		for _, c := range res.Subtasks {
			order = append(order, c.Title)
		}
		if !slices.Equal(order, []string{"Build", "Test", "Release", "Docs"}) {
			t.Errorf("plan order = %v", order)
		}

		kids := byTitle(res)
		for title, wantBlocked := range map[string]bool{"Build": false, "Docs": false, "Test": true, "Release": true} {
			c := f.get(t, kids[title].ID)
			if c.ParentID == nil || *c.ParentID != parent.ID || c.CreatorID != alice.ID || c.AssigneeID != bob.ID ||
				c.Priority != domain.PriorityHigh || c.WorkspaceID != parent.WorkspaceID {
				t.Errorf("%s = %+v", title, c)
			}
			if wantBlocked {
				if c.Status != domain.StatusBlocked || *c.BlockedReason != domain.BlockedDependency || *c.StatusBeforeBlocked != domain.StatusTodo {
					t.Errorf("%s is %s, want blocked by dependency", title, c.Status)
				}
				// Quiet: no task.blocked, no task.created, nothing at all.
				if ev := f.eventTypes(t, c.ID, 0); len(ev) != 0 {
					t.Errorf("%s sent %v", title, ev)
				}
			} else {
				if c.Status != domain.StatusTodo {
					t.Errorf("%s is %s, want todo", title, c.Status)
				}
				if got := f.outbox(t, c.ID); !slices.Equal(got, []string{"task.created>" + bob.ID}) {
					t.Errorf("%s outbox = %v", title, got)
				}
			}
			// An agent's change never queues agent work: no triage, and no
			// blocker investigation for the dependency blocks.
			if jobs := f.jobs(t, c); len(jobs) != 0 {
				t.Errorf("%s queued %v", title, jobs)
			}
		}
		if kids["Build"].Description != "Compile and package." {
			t.Errorf("description = %q", kids["Build"].Description)
		}

		for title, want := range map[string][]uuid.UUID{
			"Build": nil, "Docs": nil, "Test": {kids["Build"].ID}, "Release": {kids["Build"].ID, kids["Test"].ID},
		} {
			got, err := f.b.Deps.ListBlockers(ctx, tenant, kids[title].ID)
			if err != nil {
				t.Fatal(err)
			}
			slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
			slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
			if !slices.Equal(got, want) {
				t.Errorf("%s blockers = %v, want %v", title, got, want)
			}
		}

		// The parent records the run but keeps its version.
		stored := f.get(t, parent.ID)
		if stored.Version != parent.Version {
			t.Errorf("parent moved to v%d", stored.Version)
		}
		run, ok := stored.RunOf(job)
		if !ok || run.Agent != domain.AgentDecomposer || run.Reasoning != "Split by stage." || len(run.SubtaskIDs()) != 4 {
			t.Errorf("parent run = %+v", run)
		}
		if f.wakes.Load() == 0 {
			t.Error("the relay was not woken")
		}
	})
}

func TestCreateSubtasksIsOncePerJob(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		parent := f.create(t, nil)
		job := uuid.New()
		first := f.decompose(t, parent, job, launchPlan)
		before := f.taskCount(t)

		// A retry of the same job, even with a different plan, creates nothing.
		again := f.decompose(t, parent, job, []domain.SubtaskSpec{{Title: "Something else"}})
		if again.Created || len(again.Subtasks) != 4 || f.taskCount(t) != before {
			t.Fatalf("second call = %+v, tasks %d -> %d", again, before, f.taskCount(t))
		}
		for i := range first.Subtasks {
			if again.Subtasks[i].ID != first.Subtasks[i].ID {
				t.Errorf("subtask %d differs", i)
			}
		}
		// Another job may split the task again.
		if other := f.decompose(t, parent, uuid.New(), []domain.SubtaskSpec{{Title: "Follow-up"}}); !other.Created {
			t.Error("a second job created nothing")
		}
	})
}

func TestCreateSubtasksRejects(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.setAgents(t, domain.AgentSettingsPatch{MaxSubtasksPerDecomposition: ptr(3)})
		parent := f.create(t, nil)
		before := f.taskCount(t)
		run := domain.AgentRunRecord{JobID: uuid.New(), Agent: domain.AgentDecomposer}
		cases := []struct {
			name    string
			actor   domain.Actor
			version int
			specs   []domain.SubtaskSpec
			want    error
		}{
			{"cycle", decomposer, parent.Version, []domain.SubtaskSpec{{Title: "A", DependsOn: []string{"B"}}, {Title: "B", DependsOn: []string{"A"}}}, domain.ErrValidation},
			{"unknown title", decomposer, parent.Version, []domain.SubtaskSpec{{Title: "A", DependsOn: []string{"Z"}}}, domain.ErrValidation},
			{"over the workspace cap", decomposer, parent.Version, launchPlan, domain.ErrValidation},
			{"stale version", decomposer, parent.Version - 1, []domain.SubtaskSpec{{Title: "A"}}, domain.ErrVersionConflict},
			{"a person", alice, parent.Version, []domain.SubtaskSpec{{Title: "A"}}, domain.ErrValidation},
		}
		for _, c := range cases {
			_, err := f.svc.CreateSubtasks(ctx, c.actor, tenant, parent.ID, c.version, alice.ID, c.specs, run)
			if !errors.Is(err, c.want) {
				t.Errorf("%s: error = %v, want %v", c.name, err, c.want)
			}
		}
		if n := f.taskCount(t); n != before {
			t.Errorf("rejected plans left %d tasks behind", n-before)
		}
		if _, ok := f.get(t, parent.ID).RunOf(run.JobID); ok {
			t.Error("a rejected plan was recorded on the parent")
		}

		// A closed parent cannot be split, whether archived or completed.
		archived := f.setStatus(t, f.create(t, nil), domain.StatusArchived)
		if _, err := f.svc.CreateSubtasks(ctx, decomposer, tenant, archived.ID, archived.Version, alice.ID,
			[]domain.SubtaskSpec{{Title: "A"}}, run); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("archived parent: %v", err)
		}
		done := f.setStatus(t, parent, domain.StatusCompleted)
		if _, err := f.svc.CreateSubtasks(ctx, decomposer, tenant, parent.ID, done.Version, alice.ID,
			[]domain.SubtaskSpec{{Title: "A"}}, run); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("completed parent: %v", err)
		}
		// Another tenant's task is not found.
		if _, err := f.svc.CreateSubtasks(ctx, decomposer, "tenant_other", parent.ID, done.Version, alice.ID,
			[]domain.SubtaskSpec{{Title: "A"}}, run); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("other tenant: %v", err)
		}
	})
}

// TestCreateSubtasksIsAllOrNothing fails each repository call of a
// decomposition in turn: none may leave a subtask, a link, an outbox row, or
// a parent record behind.
func TestCreateSubtasksIsAllOrNothing(t *testing.T) {
	for failAt := 1; ; failAt++ {
		b := repotest.Memory()
		f := newFixture(t, b, nil)
		parent := f.create(t, nil)
		before, count := snapshot(t, b, []uuid.UUID{parent.ID}), f.taskCount(t)
		in := &injector{failAt: failAt}
		_, err := faultyService(b, in, f.now).CreateSubtasks(ctx, decomposer, tenant, parent.ID, parent.Version, alice.ID, launchPlan,
			domain.AgentRunRecord{JobID: uuid.New(), Agent: domain.AgentDecomposer})
		if in.calls < failAt {
			if err != nil {
				t.Fatalf("clean run failed: %v", err)
			}
			if f.taskCount(t) != count+4 {
				t.Fatalf("clean run made %d subtasks", f.taskCount(t)-count)
			}
			if failAt < 10 {
				t.Fatalf("only %d repository calls: the batch was not exercised", failAt-1)
			}
			return
		}
		if !errors.Is(err, errInjected) {
			t.Errorf("failing call %d: error = %v", failAt, err)
		}
		if n := f.taskCount(t); n != count {
			t.Errorf("failing call %d left %d subtasks behind", failAt, n-count)
		}
		if after := snapshot(t, b, []uuid.UUID{parent.ID}); after != before {
			t.Errorf("failing call %d changed the parent:\n%s%s", failAt, before, after)
		}
		if runs := f.get(t, parent.ID).AgentMetadata.Runs; len(runs) != 0 {
			t.Errorf("failing call %d recorded %+v on the parent", failAt, runs)
		}
	}
}
