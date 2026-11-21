package e2e

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/agent"
	"github.com/rtrtarun/taskify/internal/agent/agenttest"
	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
)

// TestLLMSmoke runs the reference agent against a real model: the client,
// the tool call format, the registry, and the runtime together. It is
// opt-in: skipped unless LLM_BASE_URL is set (task M5-11).
//
//	LLM_BASE_URL=http://localhost:11434/v1 LLM_MODEL=qwen2.5-coder:7b go test ./tests/e2e -run TestLLMSmoke -v
func TestLLMSmoke(t *testing.T) {
	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		t.Skip("LLM_BASE_URL is not set")
	}
	model := os.Getenv("LLM_MODEL")
	if model == "" {
		model = "qwen2.5-coder:7b"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	b := repotest.Memory()
	svc := service.NewTaskService(service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments,
		Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs}, service.Config{PublicBaseURL: "http://localhost"})
	w, _ := domain.NewWorkspace("tenant_smoke", "ws", "Smoke", time.Now())
	enabled, noApproval := true, false
	if err := w.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: &enabled, RequireHITLForAll: &noApproval}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.Workspaces.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, domain.User("usr_smoke"), domain.NewTaskInput{
		TenantID: "tenant_smoke", WorkspaceID: "ws", Title: "Checkout page crashes when the cart is empty"})
	if err != nil {
		t.Fatal(err)
	}

	client, err := llm.NewOpenAICompat(llm.Config{BaseURL: baseURL, Model: model, APIKey: os.Getenv("LLM_API_KEY"), Timeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(b.Tasks)
	if err := reg.Register(agenttest.DescribeTool{Tasks: svc}); err != nil {
		t.Fatal(err)
	}
	rt, err := agent.New(agent.Deps{Tx: b.Tx, Outbox: b.Outbox, Jobs: b.AgentJobs, Tasks: b.Tasks, Workspaces: b.Workspaces, LLM: client, Tools: reg},
		agent.Config{Workers: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, agenttest.Agent{Tasks: b.Tasks})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := rt.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}

	jobs, _ := b.AgentJobs.ListForTask(ctx, "tenant_smoke", task.ID)
	job := jobs[0]
	runs, _ := b.AgentJobs.ListRuns(ctx, job.ID)
	for _, r := range runs {
		t.Logf("turn %d: %s %s %s (%d+%d tokens, %d ms)", r.Turn, r.Outcome, r.ToolName, r.ToolArgs,
			r.PromptTokens, r.CompletionTokens, r.LatencyMS)
	}
	if job.Status != domain.JobCompleted {
		t.Fatalf("job = %s: %s", job.Status, job.ErrorMessage)
	}
	stored, _ := b.Tasks.Get(ctx, "tenant_smoke", task.ID)
	if stored.Description == "" {
		t.Errorf("the model did not call Describe; result %s", job.ExecutionResult)
	}
	t.Logf("description written by %s: %q", model, stored.Description)
}
