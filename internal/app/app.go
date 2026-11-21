// Package app assembles Taskify from its configuration: repositories,
// services, the HTTP router, and the background workers. cmd/taskify and the
// end-to-end tests both build the service through New, so they run the same
// wiring.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rtrtarun/taskify/internal/agent"
	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/api"
	"github.com/rtrtarun/taskify/internal/config"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository/postgres"
	"github.com/rtrtarun/taskify/internal/service"
	"github.com/rtrtarun/taskify/internal/worker"
)

const shutdownTimeout = 15 * time.Second

// App is a configured Taskify instance.
type App struct {
	// Handler serves the HTTP API.
	Handler http.Handler

	cfg        config.Config
	logger     *slog.Logger
	db         *postgres.DB
	borrowedDB bool
	relay      *worker.Relay
	scheduler  *worker.Scheduler
	cleanup    *worker.Cleanup
	// agents is nil unless AGENTS_ENABLED is true.
	agents *agent.Runtime
}

// New builds the service. It connects lazily, so it does not fail when
// PostgreSQL is briefly unavailable.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, version string) (*App, error) {
	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("database pool: %w", err)
	}
	a, err := assemble(db, cfg, logger, version)
	if err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

// NewWithDB builds the service on an existing pool. Close does not close it.
// Tests use it to run the service against a throwaway database.
func NewWithDB(db *postgres.DB, cfg config.Config, logger *slog.Logger, version string) (*App, error) {
	a, err := assemble(db, cfg, logger, version)
	if err != nil {
		return nil, err
	}
	a.borrowedDB = true
	return a, nil
}

func assemble(db *postgres.DB, cfg config.Config, logger *slog.Logger, version string) (*App, error) {
	tasks := postgres.NewTaskRepo(db)
	outbox := postgres.NewOutboxRepo(db)
	deliveries := postgres.NewDeliveryRepo(db)
	client := agentpulse.New(cfg.AgentPulseBaseURL, cfg.AgentPulseAPIKey)
	metrics := observability.NewMetrics(func(ctx context.Context) (int, error) {
		return outbox.CountByStatus(ctx, domain.OutboxPending)
	})

	relay := worker.NewRelay(outbox, client, worker.RelayConfig{
		Workers:      cfg.OutboxWorkers,
		BatchSize:    cfg.OutboxBatchSize,
		PollInterval: cfg.OutboxPollInterval,
		Lease:        cfg.OutboxLease,
		Logger:       logger,
		Metrics:      metrics,
	})
	taskSvc := service.NewTaskService(service.Deps{
		Tx: db, Tasks: tasks, Workspaces: postgres.NewWorkspaceRepo(db),
		Comments: postgres.NewCommentRepo(db), Outbox: outbox, Dependencies: postgres.NewDependencyRepo(db),
		AgentJobs: postgres.NewAgentJobRepo(db),
	}, service.Config{
		PublicBaseURL:     cfg.PublicBaseURL,
		OutboxMaxAttempts: cfg.OutboxMaxAttempts,
	}, service.WithWake(relay.Wake), service.WithMetrics(metrics))
	feedback := service.NewFeedbackService(tasks, client, metrics)

	handler, err := api.NewRouter(api.Deps{
		Tasks:      taskSvc,
		Workspaces: service.NewWorkspaceService(db, postgres.NewWorkspaceRepo(db), postgres.NewRuleRepo(db), client, nil),
		Feedback:   feedback,
		Webhooks:   service.NewWebhookService(db, deliveries, taskSvc, feedback, logger, nil),
		Learning:   client,
		Auth: api.AuthConfig{
			Development:         cfg.IsDevelopment(),
			Algorithm:           cfg.JWTAlgorithm,
			Secret:              cfg.JWTSecret,
			PublicKey:           cfg.JWTPublicKey,
			TrustedGatewayCIDRs: cfg.TrustedGatewayCIDRs,
		},
		WebhookSecret: cfg.AgentPulseWebhookSecret,
		RateLimit:     api.RateLimitConfig{RPS: cfg.RateLimitRPS, Burst: cfg.RateLimitBurst},
		Version:       version,
		Logger:        logger,
		Metrics:       metrics,
		Ready:         db.Ping,
	})
	if err != nil {
		return nil, err
	}

	runtime, err := newAgentRuntime(cfg, db, tasks, logger, metrics, relay.Wake)
	if err != nil {
		return nil, err
	}

	return &App{
		agents:  runtime,
		Handler: handler,
		cfg:     cfg,
		logger:  logger,
		db:      db,
		relay:   relay,
		scheduler: worker.NewScheduler(db, tasks, outbox, worker.SchedulerConfig{
			Interval:          cfg.SchedulerInterval,
			PublicBaseURL:     cfg.PublicBaseURL,
			OutboxMaxAttempts: cfg.OutboxMaxAttempts,
			Logger:            logger,
			Wake:              relay.Wake,
		}),
		cleanup: worker.NewCleanup(outbox, worker.CleanupConfig{Deliveries: deliveries, Logger: logger}),
	}, nil
}

// newAgentRuntime builds the agent runtime, or returns nil when
// AGENTS_ENABLED is false: then no job is ever claimed.
func newAgentRuntime(cfg config.Config, db *postgres.DB, tasks *postgres.TaskRepo, logger *slog.Logger,
	metrics *observability.Metrics, wake func()) (*agent.Runtime, error) {
	if !cfg.AgentsEnabled {
		return nil, nil
	}
	client, err := llm.NewOpenAICompat(llm.Config{
		BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel, Timeout: cfg.LLMTimeout,
	})
	if err != nil {
		return nil, err
	}
	// Agents and their tools are registered here as they are built (M7 on).
	// Until then the runtime claims nothing and only recovers stuck jobs.
	return agent.New(agent.Deps{
		Tx:         db,
		Outbox:     postgres.NewOutboxRepo(db),
		Jobs:       postgres.NewAgentJobRepo(db),
		Tasks:      tasks,
		Workspaces: postgres.NewWorkspaceRepo(db),
		LLM:        client,
		Tools:      tools.NewRegistry(tasks),
	}, agent.Config{
		Workers:     cfg.AgentWorkers,
		MaxTurns:    cfg.AgentMaxTurns,
		MaxAttempts: cfg.AgentMaxAttempts,
		Logger:      logger,
		Metrics:     metrics,

		PublicBaseURL:     cfg.PublicBaseURL,
		OutboxMaxAttempts: cfg.OutboxMaxAttempts,
		Wake:              wake,
	})
}

// AgentsRunning reports whether the agent runtime is part of this instance.
func (a *App) AgentsRunning() bool { return a.agents != nil }

// RunWorkers runs the relay, scheduler, cleanup, and, when AGENTS_ENABLED
// is true, the agent runtime, until ctx is cancelled.
func (a *App) RunWorkers(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.relay.Run(ctx) })
	g.Go(func() error { return a.scheduler.Run(ctx) })
	g.Go(func() error { return a.cleanup.Run(ctx) })
	if a.agents != nil {
		g.Go(func() error { return a.agents.Run(ctx) })
	}
	return g.Wait()
}

// Run serves HTTP on the configured port and runs the workers until ctx is
// cancelled, then shuts down: it stops accepting requests and lets in-flight
// requests and sends finish.
func (a *App) Run(ctx context.Context, version string) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.Port),
		Handler:           a.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.RunWorkers(gctx) })
	g.Go(func() error {
		a.logger.Info("taskify listening", "addr", srv.Addr, "version", version, "config", a.cfg.String())
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		a.logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}

// Close releases the database pool, unless it was passed to NewWithDB.
func (a *App) Close() {
	if !a.borrowedDB {
		a.db.Close()
	}
}
