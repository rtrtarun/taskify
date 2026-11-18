package service

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
)

var agentActor = domain.Actor{ID: "agent_decomposer", Kind: domain.ActorAgent}

// withMetrics gives the fixture's service a fresh metrics registry.
func (f *fixture) withMetrics() *observability.Metrics {
	m := observability.NewMetrics(nil)
	f.svc.metrics = m
	return m
}

// setAgents changes the workspace's agent settings directly.
func (f *fixture) setAgents(t *testing.T, p domain.AgentSettingsPatch) {
	t.Helper()
	w, err := f.b.Workspaces.Get(ctx, tenant, "ws_core")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.UpdateAgentSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Workspaces.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) enableAgents(t *testing.T) {
	f.setAgents(t, domain.AgentSettingsPatch{AgentsEnabled: ptr(true)})
}

// jobs returns the task's jobs as "agent@vN", oldest first.
func (f *fixture) jobs(t *testing.T, task *domain.Task) []string {
	t.Helper()
	jobs, err := f.b.AgentJobs.ListForTask(ctx, tenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, j := range jobs {
		if j.Status != domain.JobPending || j.DedupKey != domain.EventJobDedupKey(j.AgentType, task.ID, *j.BaseTaskVersion) {
			t.Errorf("job = %+v", j)
		}
		out = append(out, j.AgentType+"@v"+strconv.Itoa(*j.BaseTaskVersion))
	}
	return out
}

func skipped(t *testing.T, m *observability.Metrics, agent, reason string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != "taskify_agent_jobs_skipped_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range mm.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["agent_type"] == agent && labels["reason"] == reason {
				return mm.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func (f *fixture) edit(t *testing.T, actor domain.Actor, task *domain.Task, p domain.Patch) *domain.Task {
	t.Helper()
	out, err := f.svc.UpdateTask(ctx, actor, tenant, task.ID, f.get(t, task.ID).Version, p)
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	return out
}

func TestAgentsOffQueueNothing(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		m := f.withMetrics()
		// A new workspace has agents off.
		task := f.create(t, nil)
		f.edit(t, alice, task, domain.Patch{Title: ptr("Renamed")})
		f.edit(t, alice, task, domain.Patch{Status: ptr(domain.StatusBlocked)})
		if got := f.jobs(t, task); len(got) != 0 {
			t.Errorf("jobs with agents off = %v", got)
		}
		if n := skipped(t, m, domain.AgentTriage, observability.SkipAgentsOff); n != 2 {
			t.Errorf("triage skipped %v times, want 2", n)
		}
		if n := skipped(t, m, domain.AgentBlocker, observability.SkipAgentsOff); n != 1 {
			t.Errorf("blocker skipped %v times, want 1", n)
		}
	})
}

func TestPersonQueuesJobs(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		// Assigned to its creator: no task.created notification, but triage
		// still runs.
		task := f.create(t, func(in *domain.NewTaskInput) { in.AssigneeID = alice.ID })
		if got := f.outbox(t, task.ID); len(got) != 0 {
			t.Fatalf("outbox = %v", got)
		}
		jobs, _ := f.b.AgentJobs.ListForTask(ctx, tenant, task.ID)
		if len(jobs) != 1 {
			t.Fatalf("jobs after create = %+v", jobs)
		}
		var input jobInput
		if err := json.Unmarshal(jobs[0].InputContext, &input); err != nil || input != (jobInput{Trigger: domain.EventTaskCreated, ActorID: alice.ID}) {
			t.Errorf("input_context = %s, %v", jobs[0].InputContext, err)
		}

		f.edit(t, alice, task, domain.Patch{Title: ptr("Renamed")})             // v2: triage
		f.edit(t, alice, task, domain.Patch{AssigneeID: ptr(bob.ID)})           // v3: nothing
		f.edit(t, alice, task, domain.Patch{Status: ptr(domain.StatusBlocked)}) // v4: blocker
		want := []string{"triage@v1", "triage@v2", "blocker_investigator@v4"}
		if got := f.jobs(t, task); !slices.Equal(got, want) {
			t.Errorf("jobs = %v, want %v", got, want)
		}

		done := f.create(t, nil)
		f.setStatus(t, done, domain.StatusCompleted) // v2: nothing
		if got := f.jobs(t, done); !slices.Equal(got, []string{"triage@v1"}) {
			t.Errorf("completion queued %v", got)
		}
	})
}

func TestAgentAndSystemChangesQueueNothing(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		m := f.withMetrics()

		// An agent's own work: a task it creates, edits, and blocks.
		task, err := f.svc.CreateTask(ctx, agentActor, domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "Subtask", AssigneeID: bob.ID})
		if err != nil {
			t.Fatal(err)
		}
		f.edit(t, agentActor, task, domain.Patch{Title: ptr("Subtask, renamed")})
		f.edit(t, agentActor, task, domain.Patch{Status: ptr(domain.StatusBlocked)})
		if got := f.jobs(t, task); len(got) != 0 {
			t.Errorf("agent-authored changes queued %v", got)
		}

		// Decomposition links subtasks; the system then blocks and releases
		// them. None of that may queue the blocker investigator.
		parent := f.create(t, nil)
		sub := f.unassigned(t, "Second step")
		before := f.jobs(t, sub)
		f.link(t, sub, parent) // system dependency block
		if got := f.get(t, sub.ID); got.Status != domain.StatusBlocked {
			t.Fatalf("subtask status = %s", got.Status)
		}
		f.setStatus(t, parent, domain.StatusCompleted) // system unblock
		if got := f.get(t, sub.ID); got.Status != domain.StatusTodo {
			t.Fatalf("subtask status after release = %s", got.Status)
		}
		if got := f.jobs(t, sub); !slices.Equal(got, before) {
			t.Errorf("system changes queued jobs: %v, before %v", got, before)
		}
		if n := skipped(t, m, domain.AgentBlocker, observability.SkipAgentsOff); n != 0 {
			t.Errorf("system changes were counted as skipped jobs: %v", n)
		}
	})
}

func TestDisabledAgentIsSkipped(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.setAgents(t, domain.AgentSettingsPatch{AgentsEnabled: ptr(true), DisabledAgents: &[]string{domain.AgentTriage}})
		m := f.withMetrics()
		task := f.create(t, nil)
		f.edit(t, alice, task, domain.Patch{Status: ptr(domain.StatusBlocked)})
		if got := f.jobs(t, task); !slices.Equal(got, []string{"blocker_investigator@v2"}) {
			t.Errorf("jobs = %v", got)
		}
		if n := skipped(t, m, domain.AgentTriage, observability.SkipAgentOff); n != 1 {
			t.Errorf("disabled triage skipped %v times, want 1", n)
		}
	})
}

func TestSameEventTwiceQueuesOneJob(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		m := f.withMetrics()
		task := f.create(t, nil)
		w, err := f.b.Workspaces.Get(ctx, tenant, "ws_core")
		if err != nil {
			t.Fatal(err)
		}
		// The create already queued triage at v1; the same trigger again,
		// as a replayed event would, writes nothing.
		for range 2 {
			skips, err := f.svc.queueAgentJobs(ctx, task, w.AgentSettings, domain.CreateTriggers(alice), alice, f.now)
			if err != nil {
				t.Fatal(err)
			}
			f.svc.countSkipped(skips)
		}
		if got := f.jobs(t, task); !slices.Equal(got, []string{"triage@v1"}) {
			t.Errorf("jobs = %v", got)
		}
		if n := skipped(t, m, domain.AgentTriage, observability.SkipDuplicate); n != 2 {
			t.Errorf("duplicates counted %v times, want 2", n)
		}
	})
}

func TestNoOpChangeQueuesNothing(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		task := f.create(t, nil)
		// A patch that changes nothing raises no event and keeps the version.
		f.edit(t, alice, task, domain.Patch{Title: ptr(task.Title)})
		if got := f.jobs(t, task); !slices.Equal(got, []string{"triage@v1"}) {
			t.Errorf("jobs = %v", got)
		}
	})
}
