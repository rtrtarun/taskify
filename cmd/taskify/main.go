// Command taskify runs the Taskify API server and its background workers.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rtrtarun/taskify/internal/app"
	"github.com/rtrtarun/taskify/internal/config"
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

	a, err := app.New(ctx, cfg, logger.With("commit", commit), version)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx, version)
}
