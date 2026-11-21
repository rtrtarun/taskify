package service

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

var triageAgent = domain.Agent(domain.AgentTriage)

func TestUpdateTaskAsAgentRecordsTheChange(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		f.enableAgents(t)
		task := f.create(t, nil)
		job := uuid.New()
		est := 120
		labels := []string{"bug", "checkout"}
		got, err := f.svc.UpdateTaskAsAgent(ctx, triageAgent, tenant, task.ID, task.Version,
			domain.Patch{Labels: &labels, EstimatedMinutes: &est},
			domain.AgentRunRecord{JobID: job, Agent: domain.AgentTriage, Changes: map[string]any{"labels": labels, "estimated_minutes": est}, Reasoning: "Crash report"})
		if err != nil {
			t.Fatal(err)
		}
		stored := f.get(t, task.ID)
		if !slices.Equal(stored.Labels, labels) || *stored.EstimatedMinutes != 120 || stored.Version != got.Version {
			t.Errorf("task = %+v", stored)
		}
		runs := stored.AgentMetadata.Runs
		if len(runs) != 1 || runs[0].JobID != job || runs[0].Reasoning != "Crash report" || !runs[0].At.Equal(f.now) {
			t.Errorf("runs = %+v", runs)
		}
		// An agent's change queues no agent job.
		if jobs := f.jobs(t, task); !slices.Equal(jobs, []string{"triage@v1"}) {
			t.Errorf("jobs = %v", jobs)
		}

		// Nothing to change: nothing recorded.
		if _, err := f.svc.UpdateTaskAsAgent(ctx, triageAgent, tenant, task.ID, stored.Version, domain.Patch{Labels: &labels},
			domain.AgentRunRecord{JobID: job, Agent: domain.AgentTriage}); err != nil {
			t.Fatal(err)
		}
		if n := len(f.get(t, task.ID).AgentMetadata.Runs); n != 1 {
			t.Errorf("a no-op recorded a run: %d", n)
		}
		// A stale version loses, and only agents may use this.
		if _, err := f.svc.UpdateTaskAsAgent(ctx, triageAgent, tenant, task.ID, 1, domain.Patch{Labels: &labels}, domain.AgentRunRecord{}); !errors.Is(err, domain.ErrVersionConflict) {
			t.Errorf("stale version: %v", err)
		}
		if _, err := f.svc.UpdateTaskAsAgent(ctx, alice, tenant, task.ID, stored.Version, domain.Patch{}, domain.AgentRunRecord{}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("person as actor: %v", err)
		}
	})
}

func TestRecommendPriorityKeepsTheVersion(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		task := f.create(t, nil)
		before := f.outbox(t, task.ID)
		job := uuid.New()
		if err := f.svc.RecommendPriority(ctx, tenant, task.ID, domain.PriorityRecommendation{Value: domain.PriorityCritical, JobID: job, Reasoning: "outage"}); err != nil {
			t.Fatal(err)
		}
		stored := f.get(t, task.ID)
		if stored.Version != task.Version || !stored.UpdatedAt.Equal(task.UpdatedAt) {
			t.Errorf("the recommendation moved the task: v%d, updated %v", stored.Version, stored.UpdatedAt)
		}
		if rec := stored.AgentMetadata.PriorityRecommendation; rec == nil || rec.Value != domain.PriorityCritical || rec.JobID != job {
			t.Errorf("recommendation = %+v", rec)
		}
		if after := f.outbox(t, task.ID); !slices.Equal(after, before) {
			t.Errorf("the recommendation wrote outbox rows: %v", after)
		}
		if err := f.svc.RecommendPriority(ctx, tenant, task.ID, domain.PriorityRecommendation{Value: "urgent"}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("bad value: %v", err)
		}
		// The next notification carries the recommendation as its hint.
		f.edit(t, alice, task, domain.Patch{Title: ptr("Renamed")})
		ns := f.notifications(t, task.ID)
		if last := ns[len(ns)-1]; last.EventType != domain.EventTaskUpdated || last.PriorityHint != "critical" {
			t.Errorf("last notification = %s %s", last.EventType, last.PriorityHint)
		}
	})
}

func TestAddDependenciesIsAllOrNothing(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		dep := f.unassigned(t, "Dependent")
		a, b := f.unassigned(t, "A"), f.unassigned(t, "B")
		f.link(t, b, dep) // b depends on dep: dep -> b would be a cycle

		if _, err := f.svc.AddDependencies(ctx, tenant, dep.ID, []uuid.UUID{a.ID, b.ID}); !errors.Is(err, ErrDependencyCycle) {
			t.Fatalf("cycle in the batch: %v", err)
		}
		if blockers, _ := f.b.Deps.ListBlockers(ctx, tenant, dep.ID); len(blockers) != 0 {
			t.Errorf("a partial batch was kept: %v", blockers)
		}
		got, err := f.svc.AddDependencies(ctx, tenant, dep.ID, []uuid.UUID{a.ID})
		if err != nil || got.Status != domain.StatusBlocked {
			t.Errorf("valid batch = %+v, %v", got, err)
		}
		if same, err := f.svc.AddDependencies(ctx, tenant, dep.ID, nil); err != nil || same.ID != dep.ID {
			t.Errorf("empty batch = %v, %v", same, err)
		}
	})
}
