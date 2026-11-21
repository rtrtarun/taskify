package tools

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
)

type updateEnv struct {
	b     repotest.Backend
	svc   *service.TaskService
	reg   *Registry
	task  *domain.Task
	scope Scope
}

func newUpdateEnv(t *testing.T, hitl bool) *updateEnv {
	t.Helper()
	b := repotest.Memory()
	svc := service.NewTaskService(service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments,
		Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs}, service.Config{PublicBaseURL: "https://t"})
	w, _ := domain.NewWorkspace("tenant_acme", "ws", "WS", t0)
	if err := b.Workspaces.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, domain.User("usr_alice"), domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws",
		Title: "Checkout crashes", AssigneeID: "usr_bob", Priority: domain.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(b.Tasks)
	if err := reg.Register(UpdateTaskAttributes{Tasks: svc}); err != nil {
		t.Fatal(err)
	}
	job := &domain.AgentJob{ID: uuid.New(), TenantID: "tenant_acme", TaskID: &task.ID, AgentType: domain.AgentTriage, BaseTaskVersion: &task.Version}
	settings := domain.DefaultAgentSettings()
	settings.RequireHITLForAll = hitl
	return &updateEnv{b: b, svc: svc, reg: reg, task: task, scope: NewScope(job, settings, nil)}
}

func (e *updateEnv) stored(t *testing.T) *domain.Task {
	t.Helper()
	got, err := e.b.Tasks.Get(ctx, "tenant_acme", e.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestUpdateTaskAttributesTier1(t *testing.T) {
	e := newUpdateEnv(t, false)
	res, err := e.reg.Dispatch(ctx, e.scope, toolCall(UpdateTaskAttributesName, map[string]any{
		"task_id": e.task.ID, "labels": []string{"bug", "checkout"}, "description": "Crashes when the cart is empty.",
		"estimated_minutes": 90, "reasoning": "The title reports a crash in checkout."}))
	if err != nil || !res.Executed() || res.Tier != domain.Tier1 {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	if v, ok := res.Output.(VersionedOutput); !ok || v.TaskVersion() != 2 {
		t.Errorf("output = %#v", res.Output)
	}
	got := e.stored(t)
	if !slices.Equal(got.Labels, []string{"bug", "checkout"}) || got.Description != "Crashes when the cart is empty." ||
		*got.EstimatedMinutes != 90 || got.Priority != domain.PriorityLow || got.Version != 2 {
		t.Errorf("task = %+v", got)
	}
	runs := got.AgentMetadata.Runs
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	if r.JobID != e.scope.JobID || r.Agent != domain.AgentTriage || r.Reasoning != "The title reports a crash in checkout." ||
		r.Changes["estimated_minutes"] != 90 || r.Changes["description"] == nil {
		t.Errorf("run = %+v", r)
	}
	// The JSON stored matches the shape in section 5.7.
	raw, _ := json.Marshal(got.AgentMetadata)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	entry := doc["runs"].([]any)[0].(map[string]any)
	for _, k := range []string{"job_id", "agent", "at", "changes", "reasoning"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("stored run lacks %q: %s", k, raw)
		}
	}
}

func TestUpdateTaskAttributesPriorityIsTier2(t *testing.T) {
	e := newUpdateEnv(t, false)
	args := map[string]any{"task_id": e.task.ID, "priority": "p1_critical", "labels": []string{"outage"},
		"reasoning": "Checkout is down for every customer."}
	res, err := e.reg.Dispatch(ctx, e.scope, toolCall(UpdateTaskAttributesName, args))
	if err != nil || res.Executed() || res.Tier != domain.Tier2 {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	got := e.stored(t)
	if got.Priority != domain.PriorityLow || got.Version != 1 || len(got.Labels) != 0 {
		t.Errorf("a Tier 2 call changed the task: %+v", got)
	}
	rec := got.AgentMetadata.PriorityRecommendation
	if rec == nil || rec.Value != domain.PriorityCritical || rec.JobID != e.scope.JobID || rec.Reasoning != "Checkout is down for every customer." {
		t.Errorf("recommendation = %+v", rec)
	}

	// Approval applies the whole call, priority included, and records it.
	job := &domain.AgentJob{ID: e.scope.JobID, TenantID: "tenant_acme", TaskID: &e.task.ID, AgentType: domain.AgentTriage,
		BaseTaskVersion: ptrInt(1)}
	job.ProposedActions, _ = json.Marshal([]Proposal{*res.Proposal})
	if err := e.reg.ApplyProposals(ctx, job); err != nil {
		t.Fatal(err)
	}
	got = e.stored(t)
	if got.Priority != domain.PriorityCritical || !slices.Equal(got.Labels, []string{"outage"}) || len(got.AgentMetadata.Runs) != 1 ||
		got.AgentMetadata.Runs[0].Changes["priority"] != "p1_critical" {
		t.Errorf("after approval = %+v", got)
	}
}

func TestUpdateTaskAttributesNeedsAChange(t *testing.T) {
	e := newUpdateEnv(t, false)
	_, err := e.reg.Dispatch(ctx, e.scope, toolCall(UpdateTaskAttributesName, map[string]any{"task_id": e.task.ID, "reasoning": "nothing"}))
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("no change: %v", err)
	}
	if e.stored(t).Version != 1 {
		t.Error("the task changed")
	}
	// Under require_hitl_for_all a labels-only call is a proposal and sets
	// no recommendation.
	h := newUpdateEnv(t, true)
	res, err := h.reg.Dispatch(ctx, h.scope, toolCall(UpdateTaskAttributesName, map[string]any{"task_id": h.task.ID,
		"labels": []string{"bug"}, "reasoning": "x"}))
	if err != nil || res.Executed() || h.stored(t).AgentMetadata.PriorityRecommendation != nil {
		t.Errorf("hitl labels = %+v, %v", res, err)
	}
}

func ptrInt(v int) *int { return &v }
