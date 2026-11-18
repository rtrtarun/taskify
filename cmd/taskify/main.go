// Command taskify runs the Taskify API server and its background workers.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rtrtarun/taskify/internal/app"
	"github.com/rtrtarun/taskify/internal/config"
	"github.com/rtrtarun/taskify/internal/observability"
)

// version and commit are set at release build time with -ldflags -X; see
// .goreleaser.yaml.
var (
	version = "dev"
	commit  = "none"
)

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

	shutdownTracing, err := observability.SetupTracing(ctx, version)
	if err != nil {
		return err
	}
	defer func() {
		// Flush spans still buffered for export.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			logger.Warn("trace export did not finish", "error", err)
		}
	}()

	a, err := app.New(ctx, cfg, logger.With("commit", commit), version)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx, version)
}
