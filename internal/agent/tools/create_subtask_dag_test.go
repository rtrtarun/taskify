package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
)

type dagEnv struct {
	b      repotest.Backend
	reg    *Registry
	parent *domain.Task
	job    *domain.AgentJob
}

func newDAGEnv(t *testing.T) *dagEnv {
	t.Helper()
	b := repotest.Memory()
	svc := service.NewTaskService(service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments,
		Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs}, service.Config{PublicBaseURL: "https://t"})
	w, _ := domain.NewWorkspace("tenant_acme", "ws", "WS", t0)
	if err := b.Workspaces.Create(ctx, w); err != nil { // cap 5
		t.Fatal(err)
	}
	parent, err := svc.CreateTask(ctx, domain.User("usr_alice"), domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws",
		Title: "Launch the beta", AssigneeID: "usr_bob"})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(b.Tasks)
	if err := reg.Register(CreateSubtaskDAG{Tasks: svc}); err != nil {
		t.Fatal(err)
	}
	job := &domain.AgentJob{ID: uuid.New(), TenantID: "tenant_acme", TaskID: &parent.ID, AgentType: domain.AgentDecomposer,
		BaseTaskVersion: &parent.Version, InputContext: json.RawMessage(`{"trigger":"decompose_request","actor_id":"usr_carol"}`)}
	return &dagEnv{b: b, reg: reg, parent: parent, job: job}
}

func (e *dagEnv) scope(hitl bool) Scope {
	s := domain.DefaultAgentSettings()
	s.RequireHITLForAll = hitl
	return NewScope(e.job, s, []string{CreateSubtaskDAGName})
}

func (e *dagEnv) count(t *testing.T) int {
	t.Helper()
	page, err := e.b.Tasks.List(ctx, repository.TaskFilter{TenantID: "tenant_acme", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Tasks)
}

func dag(parent uuid.UUID, subtasks ...map[string]any) map[string]any {
	return map[string]any{"parent_task_id": parent, "subtasks": subtasks, "reasoning": "By stage."}
}

func sub(title string, dependsOn ...string) map[string]any {
	m := map[string]any{"title": title}
	if len(dependsOn) > 0 {
		m["depends_on"] = dependsOn
	}
	return m
}

func TestCreateSubtaskDAGRunsAsTier1(t *testing.T) {
	e := newDAGEnv(t)
	s := e.scope(false)
	if s.MaxSubtasks != 5 || s.RequestedBy != "usr_carol" {
		t.Fatalf("scope = %+v", s)
	}
	res, err := e.reg.Dispatch(ctx, s, toolCall(CreateSubtaskDAGName, dag(e.parent.ID, sub("Test", "Build"), sub("Build"))))
	if err != nil || !res.Executed() {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	out, ok := res.Output.(SubtaskDAGResult)
	if !ok || !out.Created || len(out.Subtasks) != 2 || out.Subtasks[0].Title != "Build" || out.Subtasks[0].Status != "todo" ||
		out.Subtasks[1].Status != "blocked" {
		t.Fatalf("output = %#v", res.Output)
	}
	if _, versioned := res.Output.(VersionedOutput); versioned {
		t.Error("the parent's version does not move, so the output must not report one")
	}
	child, err := e.b.Tasks.Get(ctx, "tenant_acme", uuid.MustParse(out.Subtasks[0].ID))
	if err != nil || child.CreatorID != "usr_carol" || *child.ParentID != e.parent.ID {
		t.Errorf("child = %+v, %v", child, err)
	}
	// The children are the parent's: later calls may target them.
	if err := e.reg.checkScope(ctx, s, map[string]any{"task_id": out.Subtasks[0].ID}); err != nil {
		t.Errorf("a created child is out of scope: %v", err)
	}
}

func TestCreateSubtaskDAGChecksBeforeProposing(t *testing.T) {
	e := newDAGEnv(t)
	other := uuid.New()
	six := make([]map[string]any, 6)
	for i := range six {
		six[i] = sub(string(rune('A' + i)))
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"over the cap", dag(e.parent.ID, six...), "allows at most 5 per decomposition. Merge or drop subtasks and call CreateSubtaskDAG again with at most 5"},
		{"cycle", dag(e.parent.ID, sub("A", "B"), sub("B", "A")), "cycle"},
		{"unknown title", dag(e.parent.ID, sub("A", "Ship")), `"Ship", which is not one of the subtasks`},
		{"another parent", dag(other, sub("A")), "out_of_scope"},
		{"no subtasks", map[string]any{"parent_task_id": e.parent.ID, "subtasks": []any{}}, "invalid_arguments"},
		{"extra field", dag(e.parent.ID, map[string]any{"title": "A", "assignee": "x"}), "invalid_arguments"},
	}
	for _, hitl := range []bool{false, true} {
		for _, c := range cases {
			_, err := e.reg.Dispatch(ctx, e.scope(hitl), toolCall(CreateSubtaskDAGName, c.args))
			var ce *CallError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("hitl=%v %s: error = %v, want a rejection containing %q", hitl, c.name, err, c.want)
			}
		}
	}
	if n := e.count(t); n != 1 {
		t.Errorf("%d tasks after rejected calls, want only the parent", n)
	}
}

func TestCreateSubtaskDAGProposalAndApply(t *testing.T) {
	e := newDAGEnv(t)
	res, err := e.reg.Dispatch(ctx, e.scope(true), toolCall(CreateSubtaskDAGName, dag(e.parent.ID, sub("Build"), sub("Ship", "Build"))))
	if err != nil || res.Executed() || res.Proposal == nil {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	if n := e.count(t); n != 1 {
		t.Fatalf("a proposal created %d tasks", n-1)
	}
	e.job.ProposedActions, _ = json.Marshal([]Proposal{*res.Proposal})
	if err := e.reg.ApplyProposals(ctx, e.job); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t); n != 3 {
		t.Errorf("%d tasks after approval, want the parent and 2 subtasks", n)
	}
	// Applying again (a retried approval) creates nothing more.
	if err := e.reg.ApplyProposals(ctx, e.job); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t); n != 3 {
		t.Errorf("a repeated apply made %d tasks", n)
	}
}

func TestCreateSubtaskDAGNeedsARequester(t *testing.T) {
	e := newDAGEnv(t)
	e.job.InputContext = json.RawMessage(`{}`)
	_, err := e.reg.Dispatch(ctx, e.scope(false), toolCall(CreateSubtaskDAGName, dag(e.parent.ID, sub("A"))))
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("error = %v", err)
	}
}
