package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/service"
)

// Deps is everything the HTTP layer needs.
type Deps struct {
	Tasks      *service.TaskService
	Workspaces *service.WorkspaceService
	Feedback   *service.FeedbackService
	Webhooks   *service.WebhookService
	// AgentJobs serves /agent-jobs. Nil leaves those routes out.
	AgentJobs *service.AgentJobService
	Learning  LearningClient

	Auth          AuthConfig
	WebhookSecret string
	RateLimit     RateLimitConfig
	Version       string
	Logger        *slog.Logger
	Now           func() time.Time
	// Metrics may be nil; /metrics is then not served.
	Metrics *observability.Metrics
	// Ready reports whether the service can do its work (the database
	// answers). nil means always ready.
	Ready func(ctx context.Context) error
}

type handlers struct {
	tasks         *service.TaskService
	workspaces    *service.WorkspaceService
	feedback      *service.FeedbackService
	webhooks      *service.WebhookService
	agentJobs     *service.AgentJobService
	learning      LearningClient
	webhookSecret string
	version       string
	logger        *slog.Logger
	now           func() time.Time
}

// NewRouter returns the HTTP handler for the whole service.
//
// Routes are under /api/v1; the task routes are also served at /tasks for
// the scripts that used the original endpoints. /healthz is unauthenticated.
//
// The router deliberately has no middleware that rewrites RemoteAddr from
// X-Forwarded-For: the trusted-gateway check depends on the real peer.
func NewRouter(d Deps) (http.Handler, error) {
	if d.WebhookSecret == "" {
		return nil, errors.New("api: a webhook secret is required")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	auth, err := newAuthenticator(d.Auth)
	if err != nil {
		return nil, err
	}
	limiter := newRateLimiter(d.RateLimit)
	h := &handlers{
		tasks: d.Tasks, workspaces: d.Workspaces, feedback: d.Feedback, webhooks: d.Webhooks, agentJobs: d.AgentJobs,
		learning: d.Learning, webhookSecret: d.WebhookSecret, version: d.Version, logger: d.Logger, now: d.Now,
	}

	r := chi.NewRouter()
	r.Use(requestContext(d.Logger, d.Metrics), recoverer(d.Logger))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		h.fail(w, r, newError(http.StatusNotFound, CodeNotFound, "no such route"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		h.fail(w, r, newError(http.StatusMethodNotAllowed, CodeInvalidRequest, "method not allowed on this route"))
	})

	// Operations endpoints: outside /api/v1 and unauthenticated. Restrict
	// /metrics at the network edge if it must not be public.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
	})
	r.Get("/readyz", readiness(d.Ready, d.Logger))
	if d.Metrics != nil {
		r.Method(http.MethodGet, "/metrics", d.Metrics.Handler())
	}

	authenticated := func(r chi.Router) {
		r.Use(auth.middleware, limiter.middleware)
	}

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/webhooks/agentpulse", h.webhook)
		r.Group(func(r chi.Router) {
			authenticated(r)
			r.Route("/tasks", h.taskRoutes)
			if h.agentJobs != nil {
				r.Route("/agent-jobs", h.agentJobRoutes)
				r.Get("/me/brief", h.getMyBrief)
			}
			r.Group(h.adminRoutes)
		})
	})

	// Legacy routes from the original Taskify, aliased to the same handlers.
	r.Group(func(r chi.Router) {
		authenticated(r)
		r.Route("/tasks", h.taskRoutes)
	})

	// The OpenTelemetry handler is outermost: it continues an incoming W3C
	// trace or starts one, so every log line and outbox row below can carry
	// the trace ID.
	return otelhttp.NewHandler(r, "http.request", otelhttp.WithPropagators(observability.Propagator)), nil
}

// readiness answers 503 while a dependency the service cannot work without
// is down. AgentPulse is not one: task writes succeed without it.
func readiness(ready func(context.Context) error, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := ready(ctx); err != nil {
				logger.WarnContext(r.Context(), "not ready", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": "database unreachable"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, h.logger, err)
}
