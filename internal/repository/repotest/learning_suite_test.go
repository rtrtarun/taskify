package repotest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// learningJob queues a learning job in the tenant for the week of at.
func learningJob(t *testing.T, b Backend, tenantID string, at time.Time) domain.AgentJob {
	t.Helper()
	job, err := domain.NewLearningJob(domain.LearningTenant{TenantID: tenantID, WorkspaceID: ws}, at)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := b.AgentJobs.Insert(ctx, job); err != nil || !ok {
		t.Fatalf("insert learning job: %v, %v", ok, err)
	}
	return job
}

func suggestion(tenantID string, job uuid.UUID, eventType string, label *string, at time.Time) domain.LearningSuggestion {
	return domain.LearningSuggestion{ID: uuid.New(), TenantID: tenantID, JobID: job,
		Pattern:            domain.EventPattern{EventType: eventType, Label: label},
		ProposedAdjustment: -10, SignalCount: 6, Rationale: "Dismissed consistently.", CreatedAt: at}
}

// TestLearningSuggestions covers M11-03's record: one suggestion per job and
// pattern, a NULL label as a pattern of its own, and the patterns recorded
// since a time, per tenant.
func TestLearningSuggestions(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		docs, ops := "docs", "ops"
		job := learningJob(t, b, tenant, t0)
		other := learningJob(t, b, "tenant_other", t0)

		for _, s := range []domain.LearningSuggestion{
			suggestion(tenant, job.ID, "task.updated", &docs, t0),
			suggestion(tenant, job.ID, "task.updated", nil, t0.Add(time.Hour)),
			suggestion(tenant, job.ID, "task.created", &ops, t0.Add(-40*24*time.Hour)),
			suggestion("tenant_other", other.ID, "task.updated", &ops, t0),
		} {
			if ok, err := b.Learning.Insert(ctx, s); err != nil || !ok {
				t.Fatalf("insert %s: %v, %v", s.Pattern, ok, err)
			}
		}
		// The same job and pattern again inserts nothing, labelled or not.
		for _, s := range []domain.LearningSuggestion{
			suggestion(tenant, job.ID, "task.updated", &docs, t0.Add(time.Minute)),
			suggestion(tenant, job.ID, "task.updated", nil, t0.Add(time.Minute)),
		} {
			if ok, err := b.Learning.Insert(ctx, s); err != nil || ok {
				t.Errorf("repeat of %s: %v, %v", s.Pattern, ok, err)
			}
		}
		// Another job may submit the same pattern.
		later := learningJob(t, b, tenant, t0.Add(7*24*time.Hour))
		if ok, err := b.Learning.Insert(ctx, suggestion(tenant, later.ID, "task.updated", &docs, t0.Add(7*24*time.Hour))); err != nil || !ok {
			t.Errorf("another job: %v, %v", ok, err)
		}
		// A suggestion needs its job.
		if _, err := b.Learning.Insert(ctx, suggestion(tenant, uuid.New(), "task.updated", &docs, t0)); err == nil {
			t.Error("a suggestion without a job was recorded")
		}

		got, err := b.Learning.PatternsSince(ctx, tenant, t0.Add(-30*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || !got[0].Equal(domain.EventPattern{EventType: "task.updated"}) ||
			!got[1].Equal(domain.EventPattern{EventType: "task.updated", Label: &docs}) {
			t.Errorf("patterns since = %v", got)
		}
		if got, _ := b.Learning.PatternsSince(ctx, tenant, t0.Add(-60*24*time.Hour)); len(got) != 3 {
			t.Errorf("patterns since 60 days = %v", got)
		}
		if got, _ := b.Learning.PatternsSince(ctx, "tenant_third", t0.Add(-60*24*time.Hour)); len(got) != 0 {
			t.Errorf("a tenant with none: %v", got)
		}
	})
}

// TestListLearningTenants covers the weekly scan's query: tenants with a
// workspace that allows the learning agent, each once with its first such
// workspace, without one already queued for the week.
func TestListLearningTenants(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		yes, no := true, false
		workspace := func(tenantID, id string, p domain.AgentSettingsPatch) {
			t.Helper()
			w, err := domain.NewWorkspace(tenantID, id, id, t0)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.UpdateAgentSettings(p, t0); err != nil {
				t.Fatal(err)
			}
			if err := b.Workspaces.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
		}
		workspace("tenant_a", "ws_z", domain.AgentSettingsPatch{AgentsEnabled: &yes})
		workspace("tenant_a", "ws_b", domain.AgentSettingsPatch{AgentsEnabled: &yes})
		workspace("tenant_a", "ws_a", domain.AgentSettingsPatch{AgentsEnabled: &no})
		workspace("tenant_b", "ws_a", domain.AgentSettingsPatch{AgentsEnabled: &yes, DisabledAgents: &[]string{domain.AgentLearning}})
		workspace("tenant_c", "ws_a", domain.AgentSettingsPatch{AgentsEnabled: &yes, DisabledAgents: &[]string{domain.AgentDigest}})
		workspace("tenant_d", "ws_a", domain.AgentSettingsPatch{AgentsEnabled: &yes})

		week := domain.LearningWeek(t0)
		got, err := b.Workspaces.ListLearningTenants(ctx, week, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := []domain.LearningTenant{{TenantID: "tenant_a", WorkspaceID: "ws_b"}, {TenantID: "tenant_c", WorkspaceID: "ws_a"},
			{TenantID: "tenant_d", WorkspaceID: "ws_a"}}
		if len(got) != len(want) {
			t.Fatalf("tenants = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("tenant %d = %v, want %v", i, got[i], want[i])
			}
		}
		if got, _ := b.Workspaces.ListLearningTenants(ctx, week, 2); len(got) != 2 || got[1].TenantID != "tenant_c" {
			t.Errorf("limit 2 = %v", got)
		}

		// A tenant with this week's job is left out; next week it is back.
		job, err := domain.NewLearningJob(domain.LearningTenant{TenantID: "tenant_a", WorkspaceID: "ws_b"}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.AgentJobs.Insert(ctx, job); err != nil {
			t.Fatal(err)
		}
		if got, _ := b.Workspaces.ListLearningTenants(ctx, week, 10); len(got) != 2 || got[0].TenantID != "tenant_c" {
			t.Errorf("after queueing tenant_a = %v", got)
		}
		next := domain.LearningWeek(t0.Add(7 * 24 * time.Hour))
		if got, _ := b.Workspaces.ListLearningTenants(ctx, next, 10); len(got) != 3 {
			t.Errorf("next week = %v", got)
		}
	})
}
