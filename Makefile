# Targets are plain commands so they run under Git Bash on Windows as well as
# on macOS and Linux.

BIN          := bin
DATABASE_URL ?= postgres://taskify:taskify@localhost:5433/taskify?sslmode=disable
JWT_SECRET   ?= dev-secret
OAPI_CODEGEN := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.1
MIGRATE      := go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.3

.PHONY: build build-embedded build-cli-all test lint migrate-up migrate-down run dev-token conformance generate

build:
	go build -o $(BIN)/taskify ./cmd/taskify
	go build -o $(BIN)/mock-agentpulse ./cmd/mock-agentpulse
	go build -o $(BIN)/mock-llm ./cmd/mock-llm
	go build -o $(BIN)/devtoken ./cmd/devtoken
	go build -o $(BIN)/taskctl ./cmd/taskctl

# The server with the web app compiled in (M14-12): serve it with
# SERVE_EMBEDDED_SPA=true. Plain `build` needs no Node toolchain.
build-embedded:
	cd web && npm ci && npm run build
	go build -tags webembed -o $(BIN)/taskify ./cmd/taskify

# taskctl for Windows, macOS, and Linux, as static binaries (M13-06). The
# release builds these, and more, through goreleaser; this is the quick check
# CI runs on every change.
build-cli-all:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -o $(BIN)/taskctl-linux-amd64       ./cmd/taskctl
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -o $(BIN)/taskctl-darwin-arm64      ./cmd/taskctl
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o $(BIN)/taskctl-windows-amd64.exe ./cmd/taskctl

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

# Regenerates pkg/client from contract/openapi.yaml (G19). CI runs this and
# fails when it changes a file, so a contract change needs the regenerated
# client in the same commit. Never edit the generated file by hand.
generate:
	$(OAPI_CODEGEN) -config pkg/client/oapi-codegen.yaml contract/openapi.yaml
