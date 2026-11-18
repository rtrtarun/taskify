// Command taskify runs the Taskify API server and its background workers.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/RONGALI-TARUN/taskify/internal/agentpulse"
	"github.com/RONGALI-TARUN/taskify/internal/config"
	"github.com/RONGALI-TARUN/taskify/internal/repository/postgres"
	"github.com/RONGALI-TARUN/taskify/internal/worker"
)

const shutdownTimeout = 15 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("taskify stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The pool connects lazily, so start-up does not fail when PostgreSQL is
	// briefly unavailable. Readiness reporting arrives with /readyz in M4.
	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer db.Close()

	tasks := postgres.NewTaskRepo(db)
	outbox := postgres.NewOutboxRepo(db)
	notifier := agentpulse.New(cfg.AgentPulseBaseURL, cfg.AgentPulseAPIKey)

	relay := worker.NewRelay(outbox, notifier, worker.RelayConfig{
		Workers:      cfg.OutboxWorkers,
		BatchSize:    cfg.OutboxBatchSize,
		PollInterval: cfg.OutboxPollInterval,
		Lease:        cfg.OutboxLease,
		Logger:       logger,
	})
	scheduler := worker.NewScheduler(db, tasks, outbox, worker.SchedulerConfig{
		Interval:          cfg.SchedulerInterval,
		PublicBaseURL:     cfg.PublicBaseURL,
		OutboxMaxAttempts: cfg.OutboxMaxAttempts,
		Logger:            logger,
		Wake:              relay.Wake,
	})
	cleanup := worker.NewCleanup(outbox, worker.CleanupConfig{Logger: logger})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           newHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)

	// Workers stop when gctx is cancelled: on a signal, or if any of them or
	// the HTTP server fails.
	g.Go(func() error { return relay.Run(gctx) })
	g.Go(func() error { return scheduler.Run(gctx) })
	g.Go(func() error { return cleanup.Run(gctx) })

	g.Go(func() error {
		logger.Info("taskify listening", "addr", srv.Addr, "config", cfg.String())
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		<-gctx.Done()
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})

	return g.Wait()
}

// newHandler returns the HTTP routes. It holds only the liveness probe for
// now; the API router in internal/api replaces it in M3.
func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	return mux
}
