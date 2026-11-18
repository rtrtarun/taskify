// Package observability holds Taskify's Prometheus metrics and OpenTelemetry
// tracing (docs/architecture.md section 6.3).
//
// A nil *Metrics is valid and records nothing, so packages can take one
// without every test having to build it.
package observability

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Dispatch outcomes, the outcome label of the dispatch histogram.
const (
	OutcomeDispatched  = "dispatched"
	OutcomeRetry       = "retry"
	OutcomeDeadLetter  = "dead_letter"
	OutcomeCircuitOpen = "circuit_open"
)

// Reasons an agent job a change asked for was not queued, the reason label
// of the skipped-jobs counter.
const (
	SkipAgentsOff = "agents_disabled" // the workspace has agents off
	SkipAgentOff  = "agent_disabled"  // this agent is in disabled_agents
	SkipDuplicate = "duplicate"       // a job with the same dedup key exists
)

// Metrics are the service's Prometheus metrics, on their own registry.
type Metrics struct {
	registry         *prometheus.Registry
	httpDuration     *prometheus.HistogramVec
	dispatchDuration *prometheus.HistogramVec
	deadLetters      *prometheus.CounterVec
	feedback         *prometheus.CounterVec
	jobsSkipped      *prometheus.CounterVec
}

// PendingCounter reports how many outbox rows are pending.
type PendingCounter func(ctx context.Context) (int, error)

// NewMetrics registers the metrics. pending is queried on each scrape for the
// outbox backlog gauge; a failing query omits the gauge from that scrape
// rather than reporting a wrong value.
func NewMetrics(pending PendingCounter) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "taskify_http_request_duration_seconds",
			Help:    "HTTP request latency by route, method, and status.",
			Buckets: []float64{.001, .0025, .005, .01, .015, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"route", "method", "status"}),
		dispatchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "taskify_outbox_dispatch_duration_seconds",
			Help:    "Time to send one outbox row to AgentPulse, by event type and outcome.",
			Buckets: []float64{.01, .025, .05, .1, .15, .25, .4, .75, 1, 2.5, 5, 10},
		}, []string{"event_type", "outcome"}),
		deadLetters: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "taskify_outbox_dlq_total",
			Help: "Outbox rows moved to dead letter, by event type.",
		}, []string{"event_type"}),
		feedback: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "taskify_feedback_forwarded_total",
			Help: "Feedback signals forwarded to AgentPulse, by signal.",
		}, []string{"signal"}),
		jobsSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "taskify_agent_jobs_skipped_total",
			Help: "Agent jobs a task change asked for but that were not queued, by agent type and reason.",
		}, []string{"agent_type", "reason"}),
	}
	reg.MustRegister(m.httpDuration, m.dispatchDuration, m.deadLetters, m.feedback, m.jobsSkipped,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if pending != nil {
		reg.MustRegister(&pendingCollector{count: pending})
	}
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry, for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveHTTP records one request.
func (m *Metrics) ObserveHTTP(route, method string, status int, d time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched" // keeps arbitrary paths out of the label set
	}
	m.httpDuration.WithLabelValues(route, method, statusLabel(status)).Observe(d.Seconds())
}

// ObserveDispatch records one send attempt and counts dead letters.
func (m *Metrics) ObserveDispatch(eventType, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.dispatchDuration.WithLabelValues(eventType, outcome).Observe(d.Seconds())
	if outcome == OutcomeDeadLetter {
		m.deadLetters.WithLabelValues(eventType).Inc()
	}
}

// FeedbackForwarded counts one signal forwarded to AgentPulse.
func (m *Metrics) FeedbackForwarded(signal string) {
	if m == nil {
		return
	}
	m.feedback.WithLabelValues(signal).Inc()
}

// AgentJobSkipped counts one agent job that was not queued.
func (m *Metrics) AgentJobSkipped(agentType, reason string) {
	if m == nil {
		return
	}
	m.jobsSkipped.WithLabelValues(agentType, reason).Inc()
}

func statusLabel(code int) string {
	const digits = "0123456789"
	if code < 100 || code > 999 {
		return "unknown"
	}
	return string([]byte{digits[code/100], digits[code/10%10], digits[code%10]})
}

var pendingDesc = prometheus.NewDesc("taskify_outbox_pending_rows",
	"Outbox rows waiting to be sent to AgentPulse.", nil, nil)

// pendingCollector queries the backlog at scrape time, so the gauge is
// right whichever replica is scraped.
type pendingCollector struct {
	count PendingCounter
}

func (c *pendingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- pendingDesc }

func (c *pendingCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := c.count(ctx)
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(pendingDesc, prometheus.GaugeValue, float64(n))
}
