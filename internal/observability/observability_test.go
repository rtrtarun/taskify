package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func TestMetricsExposition(t *testing.T) {
	pending := 7
	m := NewMetrics(func(context.Context) (int, error) { return pending, nil })

	m.ObserveHTTP("/api/v1/tasks/{id}", "GET", 200, 3*time.Millisecond)
	m.ObserveHTTP("", "GET", 404, time.Millisecond)
	m.ObserveDispatch("task.created", OutcomeDispatched, 40*time.Millisecond)
	m.ObserveDispatch("task.created", OutcomeDeadLetter, 10*time.Millisecond)
	m.ObserveDispatch("task.blocked", OutcomeRetry, 10*time.Millisecond)
	m.FeedbackForwarded("acted")
	m.FeedbackForwarded("acted")
	m.AgentJobSkipped("triage", SkipAgentsOff)
	m.ObserveAgentJob("triage", JobOutcomeCompleted, 2*time.Second)
	m.AgentTokens("triage", 120, 18)
	m.AgentTokens("triage", 80, 2)
	m.AgentProposals("triage", DecisionProposed, 2)
	m.AgentProposals("triage", DecisionApproved, 0) // nothing to count
	var none *Metrics
	none.ObserveAgentJob("triage", JobOutcomeFailed, time.Second)
	none.AgentTokens("triage", 1, 1)
	none.AgentProposals("triage", DecisionRejected, 1)

	if got := testutil.ToFloat64(m.deadLetters.WithLabelValues("task.created")); got != 1 {
		t.Errorf("dlq{task.created} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.deadLetters.WithLabelValues("task.blocked")); got != 0 {
		t.Errorf("a retry counted as a dead letter: %v", got)
	}
	if got := testutil.ToFloat64(m.feedback.WithLabelValues("acted")); got != 2 {
		t.Errorf("feedback{acted} = %v, want 2", got)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`taskify_http_request_duration_seconds_count{method="GET",route="/api/v1/tasks/{id}",status="200"} 1`,
		`taskify_http_request_duration_seconds_count{method="GET",route="unmatched",status="404"} 1`,
		`taskify_outbox_dispatch_duration_seconds_count{event_type="task.created",outcome="dispatched"} 1`,
		`taskify_outbox_dlq_total{event_type="task.created"} 1`,
		`taskify_feedback_forwarded_total{signal="acted"} 2`,
		`taskify_outbox_pending_rows 7`,
		`taskify_agent_jobs_skipped_total{agent_type="triage",reason="agents_disabled"} 1`,
		`taskify_agent_job_duration_seconds_count{agent_type="triage",outcome="completed"} 1`,
		`taskify_agent_llm_tokens_total{agent_type="triage",direction="prompt"} 200`,
		`taskify_agent_llm_tokens_total{agent_type="triage",direction="completion"} 20`,
		`taskify_agent_proposals_total{agent_type="triage",decision="proposed"} 2`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %q", want)
		}
	}

	// A failing backlog query drops the gauge rather than reporting a wrong value.
	failing := NewMetrics(func(context.Context) (int, error) { return 0, errors.New("db down") })
	rec = httptest.NewRecorder()
	failing.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), "taskify_outbox_pending_rows ") {
		t.Error("the pending gauge was reported although its query failed")
	}
	if failing.Registry() == nil || NewMetrics(nil).Registry() == nil {
		t.Error("registry missing")
	}
}

func TestNilMetricsAreSafe(t *testing.T) {
	var m *Metrics
	m.ObserveHTTP("/x", "GET", 200, time.Millisecond)
	m.ObserveDispatch("e", OutcomeDeadLetter, time.Millisecond)
	m.FeedbackForwarded("acted")
}

func TestStatusLabel(t *testing.T) {
	for code, want := range map[int]string{200: "200", 404: "404", 503: "503", 0: "unknown", 1000: "unknown"} {
		if got := statusLabel(code); got != want {
			t.Errorf("statusLabel(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestTraceContextRoundTrip(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := ExtractTraceContext(context.Background(), map[string]string{"traceparent": tp})
	if got := TraceID(ctx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("TraceID = %q", got)
	}
	if got := InjectTraceContext(ctx)["traceparent"]; got != tp {
		t.Errorf("injected %q, want %q", got, tp)
	}
	if got := InjectTraceContext(context.Background()); len(got) != 0 {
		t.Errorf("a context without a trace injected %v", got)
	}
	if ExtractTraceContext(context.Background(), nil) != context.Background() || TraceID(context.Background()) != "" {
		t.Error("empty trace handling")
	}
}

func TestSetupTracingWithoutExporter(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	shutdown, err := SetupTracing(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	// Without an exporter, new spans still get real trace IDs.
	ctx, span := Tracer().Start(context.Background(), "root")
	if !trace.SpanContextFromContext(ctx).IsValid() || TraceID(ctx) == "" {
		t.Error("a root span has no trace ID")
	}
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestSetupTracingWithExporter(t *testing.T) {
	// An endpoint that answers every export with 404: export fails, but
	// setup and shutdown must still work.
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	shutdown, err := SetupTracing(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := Tracer().Start(context.Background(), "exported")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = shutdown(ctx) // reports the failed export; nothing else to check
}
