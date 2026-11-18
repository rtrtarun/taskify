package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/RONGALI-TARUN/taskify/internal/service"
)

// Deps is everything the HTTP layer needs.
type Deps struct {
	Tasks      *service.TaskService
	Workspaces *service.WorkspaceService
	Feedback   *service.FeedbackService
	Webhooks   *service.WebhookService
	Learning   LearningClient

	Auth          AuthConfig
	WebhookSecret string
	RateLimit     RateLimitConfig
	Version       string
	Logger        *slog.Logger
	Now           func() time.Time
}

type handlers struct {
	tasks         *service.TaskService
	workspaces    *service.WorkspaceService
	feedback      *service.FeedbackService
	webhooks      *service.WebhookService
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
		tasks: d.Tasks, workspaces: d.Workspaces, feedback: d.Feedback, webhooks: d.Webhooks,
		learning: d.Learning, webhookSecret: d.WebhookSecret, version: d.Version, logger: d.Logger, now: d.Now,
	}

	r := chi.NewRouter()
	r.Use(requestContext(d.Logger), recoverer(d.Logger))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		h.fail(w, r, newError(http.StatusNotFound, CodeNotFound, "no such route"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		h.fail(w, r, newError(http.StatusMethodNotAllowed, CodeInvalidRequest, "method not allowed on this route"))
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
	})

	authenticated := func(r chi.Router) {
		r.Use(auth.middleware, limiter.middleware)
	}

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/webhooks/agentpulse", h.webhook)
		r.Group(func(r chi.Router) {
			authenticated(r)
			r.Route("/tasks", h.taskRoutes)
			r.Group(h.adminRoutes)
		})
	})

	// Legacy routes from the original Taskify, aliased to the same handlers.
	r.Group(func(r chi.Router) {
		authenticated(r)
		r.Route("/tasks", h.taskRoutes)
	})
	return r, nil
}

func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, h.logger, err)
}
