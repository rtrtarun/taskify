# Targets are plain commands so they run under Git Bash on Windows as well as
# on macOS and Linux.

BIN          := bin
DATABASE_URL ?= postgres://taskify:taskify@localhost:5433/taskify?sslmode=disable
JWT_SECRET   ?= dev-secret
MIGRATE      := go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.3

.PHONY: build test lint migrate-up migrate-down run dev-token conformance

build:
	go build -o $(BIN)/taskify ./cmd/taskify
	go build -o $(BIN)/mock-agentpulse ./cmd/mock-agentpulse
	go build -o $(BIN)/mock-llm ./cmd/mock-llm
	go build -o $(BIN)/devtoken ./cmd/devtoken

test:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

migrate-up:
	$(MIGRATE) -path migrations -database "$(DATABASE_URL)" up

migrate-down:
	$(MIGRATE) -path migrations -database "$(DATABASE_URL)" down 1

run:
	ENV=development go run ./cmd/taskify

# Usage: make dev-token ARGS="-tenant tenant_acme -user usr_1 -role member"
dev-token:
	@JWT_SECRET=$(JWT_SECRET) go run ./cmd/devtoken $(ARGS)

# Runs the conformance fixtures against a running server; see
# contract/fixtures/README.md for the settings the target needs.
# Usage: make conformance TARGET_URL=http://127.0.0.1:8091
conformance:
	TARGET_URL=$(TARGET_URL) go test -count=1 -v ./tests/conformance/...
