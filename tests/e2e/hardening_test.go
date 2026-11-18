package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"

	"github.com/rtrtarun/taskify/internal/app"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository/postgres"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

// seedOutbox writes n pending notifications with distinct keys.
func seedOutbox(t *testing.T, db *postgres.DB, n int) {
	t.Helper()
	agg := uuid.New()
	var entries []domain.OutboxEntry
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("taskify:%s:%s:task.updated:v%d", tenant, agg, i)
		body, _ := json.Marshal(domain.Notification{
			IdempotencyKey: key, TenantID: tenant, UserID: "usr_bob", Source: "taskify",
			EventType: domain.EventTaskUpdated, Title: "t", Body: "b", PriorityHint: "medium", Payload: map[string]any{},
		})
		entries = append(entries, domain.OutboxEntry{
			ID: uuid.New(), TenantID: tenant, AggregateID: agg, EventType: domain.EventTaskUpdated,
			IdempotencyKey: key, Payload: body, MaxAttempts: 10,
			NextAttemptAt: time.Now().Add(-time.Second), CreatedAt: time.Now(),
		})
	}
	if _, err := postgres.NewOutboxRepo(db).Insert(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *postgres.DB, s domain.OutboxStatus) int {
	t.Helper()
	n, err := postgres.NewOutboxRepo(db).CountByStatus(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// buildServer compiles cmd/taskify into a temporary directory.
func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "taskify")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "github.com/rtrtarun/taskify/cmd/taskify")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// TestCrashMidDispatch is the M4 exit criterion: killing Taskify mid-dispatch
// and restarting it loses no notification and delivers none twice.
//
// The process is killed outright, with no chance to clean up, while sends are
// in flight. Rows it had claimed stay in processing until the restarted
// process's lease reaper returns them. Some of those were already accepted by
// AgentPulse; they are sent again with the same idempotency key, and
// AgentPulse acknowledges them as duplicates instead of notifying twice.
func TestCrashMidDispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the server binary")
	}
	dbURL, db := repotest.NewDatabaseURL(t)
	bin := buildServer(t)

	mock := mockagentpulse.New()
	mock.SetDelay("POST", "/notify", 40*time.Millisecond)
	apSrv := httptest.NewServer(mock)
	t.Cleanup(apSrv.Close)

	const rows = 120
	seedOutbox(t, db, rows)

	startServer := func() *exec.Cmd {
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(),
			"ENV=development", "PORT="+strconv.Itoa(freePort(t)), "DATABASE_URL="+dbURL,
			"PUBLIC_BASE_URL=http://localhost", "AGENTPULSE_BASE_URL="+apSrv.URL,
			"AGENTPULSE_API_KEY=e2e", "AGENTPULSE_WEBHOOK_SECRET=e2e",
			"OUTBOX_WORKERS=4", "OUTBOX_BATCH_SIZE=8", "OUTBOX_POLL_INTERVAL=20ms", "OUTBOX_LEASE=1s",
			"OTEL_EXPORTER_OTLP_ENDPOINT=", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=",
		)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}

	first := startServer()
	waitUntil(t, "the first process to deliver some rows", 30*time.Second, func() bool {
		return len(mock.Notifications()) >= 20
	})
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	if done := count(t, db, domain.OutboxDispatched); done == rows {
		t.Fatal("everything was delivered before the kill; the test proved nothing")
	}
	t.Logf("killed with %d/%d dispatched, %d processing", count(t, db, domain.OutboxDispatched), rows, count(t, db, domain.OutboxProcessing))

	second := startServer()
	t.Cleanup(func() { _ = second.Process.Kill(); _ = second.Wait() })
	waitUntil(t, "every row to be dispatched after the restart", 60*time.Second, func() bool {
		return count(t, db, domain.OutboxDispatched) == rows
	})

	if n := len(mock.Notifications()); n != rows {
		t.Errorf("AgentPulse accepted %d distinct notifications, want %d: none lost, none doubled", n, rows)
	}
	if dl := count(t, db, domain.OutboxDeadLetter); dl != 0 {
		t.Errorf("%d rows dead-lettered", dl)
	}
	t.Logf("resends acknowledged as duplicates: %d", mock.Duplicates())
}

// TestGracefulShutdown: on shutdown, sends already in flight finish and
// their results are recorded, so nothing is left claimed.
func TestGracefulShutdown(t *testing.T) {
	db := repotest.NewDatabase(t)
	mock := mockagentpulse.New()
	mock.SetDelay("POST", "/notify", 200*time.Millisecond)
	apSrv := httptest.NewServer(mock)
	t.Cleanup(apSrv.Close)
	seedOutbox(t, db, 40)

	cfg := testConfig(apSrv.URL)
	cfg.Port = freePort(t)
	a, err := app.NewWithDB(db, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, "e2e") }()

	waitUntil(t, "sends to start", 10*time.Second, func() bool { return len(mock.Requests()) > 0 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not finish")
	}

	if n := count(t, db, domain.OutboxProcessing); n != 0 {
		t.Errorf("%d rows left claimed after a graceful shutdown", n)
	}
	if d, sent := count(t, db, domain.OutboxDispatched), len(mock.Notifications()); d != sent {
		t.Errorf("%d rows marked dispatched, but AgentPulse accepted %d", d, sent)
	}
}

// TestTraceWithSDK: with the real tracer provider installed and no incoming
// trace, the server starts one, and the same trace ID reaches AgentPulse.
func TestTraceWithSDK(t *testing.T) {
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := observability.SetupTracing(context.Background(), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	h := start(t)
	h.call("POST", "/api/v1/workspaces", "usr_admin", "admin", map[string]any{"id": "ws_e2e", "name": "E2E"}, nil)
	_, task := h.call("POST", "/api/v1/tasks", "usr_bob", "member", map[string]any{
		"workspace_id": "ws_e2e", "title": "Traced", "assignee_id": "usr_carol",
	}, nil)
	id := uuid.MustParse(task["id"].(string))
	h.waitForNotify("task.created", "usr_carol")

	rows, err := postgres.NewOutboxRepo(h.db).ListForAggregate(context.Background(), id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outbox rows = %d, %v", len(rows), err)
	}
	stored := traceIDOf(rows[0].TraceContext["traceparent"])
	if stored == "" {
		t.Fatal("the server started no trace")
	}
	if sent := traceIDOf(h.notifyHeader("task.created", "traceparent")); sent != stored {
		t.Errorf("AgentPulse got trace %q, the request had %q", sent, stored)
	}
}

func traceIDOf(traceparent string) string {
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		return ""
	}
	return parts[1]
}
