// Package localserver starts Taskify in-process on memory repositories for
// tests of the clients (the CLI today). It is the API router the real server
// uses, with a mock AgentPulse behind it, so a test needs no running process
// and no database.
package localserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/agenttest"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/api"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

const (
	// Tenant and Workspace exist from the start.
	Tenant    = "tenant_cli"
	Workspace = "ws_core"

	jwtSecret     = "localserver-jwt-secret"
	webhookSecret = "localserver-webhook-secret"
)

// Server is a running local Taskify.
type Server struct {
	URL     string
	Backend repotest.Backend
	Mock    *mockagentpulse.Server
	Tasks   *service.TaskService
}

// Start runs the server until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	b := repotest.Memory()
	mock := mockagentpulse.New()
	ap := httptest.NewServer(mock)
	t.Cleanup(ap.Close)
	apClient := agentpulse.New(ap.URL, "k", agentpulse.WithBreaker(nil))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := func() time.Time { return time.Now().UTC() }

	tasks := service.NewTaskService(service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments,
		Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs},
		service.Config{PublicBaseURL: "https://taskify.test"})
	feedback := service.NewFeedbackService(b.Tasks, apClient, nil)
	// One Tier 2 tool, so a proposal can be stored and approved.
	registry := tools.NewRegistry(b.Tasks)
	if err := registry.Register(agenttest.DescribeTool{Tasks: tasks, Tier2: true}); err != nil {
		t.Fatal(err)
	}
	agentJobs := service.NewAgentJobService(b.Tx, b.AgentJobs, b.Tasks, registry, nil, clock)
	h, err := api.NewRouter(api.Deps{
		AgentJobs:     agentJobs,
		Outbox:        service.NewOutboxService(b.Outbox, clock, nil),
		Contacts:      service.NewContactService(b.Tx, b.Contacts, b.Outbox, 10, clock, nil),
		Tasks:         tasks,
		Workspaces:    service.NewWorkspaceService(b.Tx, b.Workspaces, b.Rules, apClient, clock),
		Feedback:      feedback,
		Webhooks:      service.NewWebhookService(b.Tx, b.Deliveries, tasks, feedback, quiet, clock).WithAgentJobs(agentJobs),
		Learning:      apClient,
		Auth:          api.AuthConfig{Algorithm: "HS256", Secret: jwtSecret},
		WebhookSecret: webhookSecret,
		RateLimit:     api.RateLimitConfig{RPS: 1000, Burst: 1000},
		Version:       "localserver",
		Logger:        quiet,
		Now:           clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if _, err := tasks.CreateWorkspace(context.Background(), Tenant, Workspace, "Core"); err != nil {
		t.Fatal(err)
	}
	return &Server{URL: srv.URL, Backend: b, Mock: mock, Tasks: tasks}
}

// Token returns a JWT for a user of the server's tenant. role is "admin" or
// "member".
func (s *Server) Token(t testing.TB, user, role string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": user, "tenant_id": Tenant, "role": role, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(jwtSecret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Proposal stores a job awaiting approval that, once approved, sets the
// task's description (a Tier 2 change), based on the task's current version.
func (s *Server) Proposal(t testing.TB, taskID uuid.UUID, description string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	task, err := s.Backend.Tasks.Get(ctx, Tenant, taskID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := domain.NewAgentJob(domain.NewAgentJobInput{TenantID: Tenant, TaskID: &taskID, AgentType: domain.AgentTriage,
		DedupKey: "localserver:" + uuid.NewString(), BaseTaskVersion: &task.Version}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backend.AgentJobs.Insert(ctx, job); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(agenttest.DescribeArgs{TaskID: taskID, Description: description})
	job.ProposedActions, _ = json.Marshal([]tools.Proposal{{Tool: agenttest.DescribeToolName, Arguments: args, CallID: "c1"}})
	prev := job.UpdatedAt
	job.Status = domain.JobAwaitingApproval
	if err := s.Backend.AgentJobs.Update(ctx, &job, domain.JobPending, prev); err != nil {
		t.Fatal(err)
	}
	return job.ID
}

// DeadLetter moves the oldest pending outbox row to dead_letter and returns
// its ID.
func (s *Server) DeadLetter(t testing.TB) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	rows, err := s.Backend.Outbox.ClaimBatch(ctx, time.Now().UTC(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %d rows, %v", len(rows), err)
	}
	if err := s.Backend.Outbox.MarkDeadLetter(ctx, rows[0].ID, *rows[0].ClaimedAt, 10, "503 from AgentPulse"); err != nil {
		t.Fatal(err)
	}
	return rows[0].ID
}

// EnableAgents switches agents on for the workspace, as an admin would, so
// that a decomposition can be queued.
func (s *Server) EnableAgents(t testing.TB) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, s.URL+"/api/v1/workspaces/"+Workspace+"/settings",
		strings.NewReader(`{"agent_settings":{"agents_enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token(t, "usr_admin", "admin"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable agents: HTTP %d", resp.StatusCode)
	}
}
