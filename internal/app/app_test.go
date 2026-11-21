package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/rtrtarun/taskify/internal/config"
	"github.com/rtrtarun/taskify/internal/repository/postgres"
)

func TestAgentRuntimeOnlyWhenEnabled(t *testing.T) {
	// The pool connects lazily, so no database is needed to assemble.
	db, err := postgres.Open(context.Background(), "postgres://u:p@127.0.0.1:1/taskify?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{
		Env: config.EnvDevelopment, PublicBaseURL: "https://t", AgentPulseBaseURL: "http://ap",
		JWTAlgorithm: config.JWTAlgHS256, RateLimitRPS: 1, RateLimitBurst: 1, AgentPulseWebhookSecret: "whsec",
	}

	off, err := NewWithDB(db, cfg, logger, "test")
	if err != nil {
		t.Fatal(err)
	}
	if off.AgentsRunning() {
		t.Error("the agent runtime is built although AGENTS_ENABLED is false")
	}

	cfg.AgentsEnabled, cfg.LLMBaseURL, cfg.LLMModel = true, "http://ollama:11434/v1", "qwen2.5-coder:7b"
	on, err := NewWithDB(db, cfg, logger, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !on.AgentsRunning() {
		t.Error("the agent runtime is missing although AGENTS_ENABLED is true")
	}
}
