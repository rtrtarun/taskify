// Package bench measures the three latency targets in docs/architecture.md
// section 6.2 against PostgreSQL, through the fully assembled service.
//
//	DATABASE_URL=... go test -run '^$' -bench . -benchtime 2000x ./tests/bench/
//
// Each benchmark reports P50, P95, and P99 in milliseconds next to Go's mean.
// Requests are served in-process (no network hop), so the HTTP figures are
// handler latency. See docs/benchmarks.md for recorded results.
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rtrtarun/taskify/internal/app"
	"github.com/rtrtarun/taskify/internal/config"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

const (
	jwtSecret = "bench-secret"
	tenant    = "tenant_bench"
)

type env struct {
	b       *testing.B
	app     *app.App
	mock    *mockagentpulse.Server
	member  string
	admin   string
	cancel  context.CancelFunc
	workers chan error
}

func setup(b *testing.B, runWorkers bool) *env {
	b.Helper()
	db := repotest.NewDatabase(b)
	mock := mockagentpulse.New()
	ap := httptest.NewServer(mock)
	b.Cleanup(ap.Close)

	cfg := config.Config{
		Env: config.EnvProduction, PublicBaseURL: "https://taskify.bench",
		AgentPulseBaseURL: ap.URL, AgentPulseAPIKey: "k", AgentPulseWebhookSecret: "w",
		OutboxWorkers: 8, OutboxBatchSize: 100, OutboxPollInterval: 200 * time.Millisecond,
		OutboxMaxAttempts: 10, OutboxLease: time.Minute, SchedulerInterval: time.Hour,
		JWTAlgorithm: config.JWTAlgHS256, JWTSecret: jwtSecret, RateLimitRPS: 1e6, RateLimitBurst: 1e6,
	}
	a, err := app.NewWithDB(db, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "bench")
	if err != nil {
		b.Fatal(err)
	}
	e := &env{b: b, app: a, mock: mock, member: token(b, "usr_bob", "member"), admin: token(b, "usr_admin", "admin")}
	if runWorkers {
		ctx, cancel := context.WithCancel(context.Background())
		e.cancel, e.workers = cancel, make(chan error, 1)
		go func() { e.workers <- a.RunWorkers(ctx) }()
		b.Cleanup(func() {
			cancel()
			if err := <-e.workers; err != nil {
				b.Errorf("workers: %v", err)
			}
		})
	}
	if code, body := e.call("POST", "/api/v1/workspaces", e.admin, map[string]any{"id": "ws", "name": "Bench"}); code != 201 {
		b.Fatalf("workspace: %d %s", code, body)
	}
	return e
}

func token(b *testing.B, user, role string) string {
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": user, "tenant_id": tenant, "role": role, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(jwtSecret))
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func (e *env) call(method, path, tok string, body any) (int, []byte) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	e.app.Handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// report adds percentile metrics to the benchmark result.
func report(b *testing.B, d []time.Duration) {
	slices.Sort(d)
	at := func(p float64) float64 {
		return float64(d[int(p*float64(len(d)-1))].Microseconds()) / 1000
	}
	b.ReportMetric(at(0.50), "p50-ms")
	b.ReportMetric(at(0.95), "p95-ms")
	b.ReportMetric(at(0.99), "p99-ms")
}

// BenchmarkWritePath creates a task: one transaction writing the task and
// its outbox row. Target: P95 under 15 ms.
func BenchmarkWritePath(b *testing.B) {
	e := setup(b, false)
	body := map[string]any{"workspace_id": "ws", "title": "Benchmark task", "assignee_id": "usr_carol", "labels": []string{"bench"}}
	lat := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if code, out := e.call("POST", "/api/v1/tasks", e.member, body); code != http.StatusCreated {
			b.Fatalf("create: %d %s", code, out)
		}
		lat = append(lat, time.Since(start))
	}
	b.StopTimer()
	report(b, lat)
}

// BenchmarkReadPath lists a page of one assignee's open tasks out of 5,000.
// Target: P95 under 8 ms.
func BenchmarkReadPath(b *testing.B) {
	e := setup(b, false)
	for i := 0; i < 5000; i++ {
		assignee := fmt.Sprintf("usr_%d", i%50)
		if code, out := e.call("POST", "/api/v1/tasks", e.member, map[string]any{"workspace_id": "ws", "title": "t", "assignee_id": assignee}); code != 201 {
			b.Fatalf("seed: %d %s", code, out)
		}
	}
	lat := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := fmt.Sprintf("/api/v1/tasks?assignee_id=usr_%d&status=todo&limit=50", i%50)
		start := time.Now()
		if code, out := e.call("GET", path, e.member, nil); code != http.StatusOK {
			b.Fatalf("list: %d %s", code, out)
		}
		lat = append(lat, time.Since(start))
	}
	b.StopTimer()
	report(b, lat)
}

// BenchmarkDispatch measures from the create request returning (the change
// committed) to AgentPulse receiving the notification, with the relay
// running as in production. Target: P95 under 150 ms.
func BenchmarkDispatch(b *testing.B) {
	e := setup(b, true)
	lat := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		before := len(e.mock.Notifications())
		code, out := e.call("POST", "/api/v1/tasks", e.member, map[string]any{"workspace_id": "ws", "title": "t", "assignee_id": "usr_carol"})
		if code != http.StatusCreated {
			b.Fatalf("create: %d %s", code, out)
		}
		start := time.Now()
		for len(e.mock.Notifications()) == before {
			if time.Since(start) > 10*time.Second {
				b.Fatal("notification never arrived")
			}
			time.Sleep(200 * time.Microsecond)
		}
		lat = append(lat, time.Since(start))
	}
	b.StopTimer()
	report(b, lat)
}
