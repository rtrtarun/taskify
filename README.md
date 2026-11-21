# Taskify

A multi-tenant task service in Go that hands every notification decision to **AgentPulse**. Taskify records what happened to a task; AgentPulse decides who is told, when, and on which channel, so people get one alert for what matters instead of twenty for everything.

Task changes are written with their notifications in a single PostgreSQL transaction (a transactional outbox), and a background relay delivers them. The API never waits on AgentPulse, and an AgentPulse outage delays notifications without losing them.

| Document | Covers |
| --- | --- |
| [docs/features.md](docs/features.md) | What Taskify does and why |
| [docs/architecture.md](docs/architecture.md) | How it is built |
| [docs/milestones.md](docs/milestones.md) and [docs/implementation/](docs/implementation/README.md) | Delivery plan and progress |
| [docs/releasing.md](docs/releasing.md) | Versioning and releases |
| [docs/benchmarks.md](docs/benchmarks.md) | Measured latency |

## How it works

```mermaid
flowchart LR
    client["Client: CLI, web, scripts"] -->|"REST /api/v1"| api["HTTP API"]
    api -->|"one transaction"| pg[("PostgreSQL: tasks + outbox")]
    sched["Deadline scheduler"] --> pg
    relay["Outbox relay"] -->|"claim rows"| pg
    relay -->|"/notify"| ap["AgentPulse"]
    ap -->|"Slack, email, in-app"| user["People"]
    user -->|"Mark done, Snooze"| ap
    ap -->|"signed webhook"| api
```

- **Write path:** a request changes a task and inserts one outbox row per notification in the same transaction.
- **Relay:** claims rows with `FOR UPDATE SKIP LOCKED`, sends them, and retries with backoff; after 10 failures a row is parked as a dead letter. Each notification carries a deterministic idempotency key, so a resend is never delivered twice.
- **Scheduler:** writes `task.due_soon` and `task.overdue` reminders. Any number of replicas can run.
- **Webhook:** AgentPulse calls back when someone acts on a notification, and Taskify applies the action.

## Run it locally

Requirements: Go 1.26 and Docker.

```bash
docker compose up -d --build                  # PostgreSQL, a mock AgentPulse, Taskify
make migrate-up                               # apply migrations to the compose database
curl localhost:8080/healthz                   # {"status":"ok","version":"dev"}
```

The compose database is published on host port **5433**, so it does not collide with a local PostgreSQL on 5432. The mock AgentPulse on port 9090 records every call; `GET localhost:9090/_mock/requests` shows them.

Compose runs Taskify with `ENV=development`, which accepts identity headers instead of a token:

```bash
H='-H X-Tenant-ID:tenant_dev -H X-User-ID:usr_admin -H X-User-Role:admin -H Content-Type:application/json'

curl -X POST localhost:8080/api/v1/workspaces $H -d '{"id":"ws_core","name":"Core"}'
curl -X POST localhost:8080/api/v1/tasks $H \
  -d '{"workspace_id":"ws_core","title":"Fix checkout crash","priority":"p1_critical","assignee_id":"usr_bob"}'
curl localhost:8080/api/v1/tasks $H
```

To use a token instead, `make dev-token ARGS="-tenant tenant_dev -user usr_bob -role member"` prints a signed development JWT for `Authorization: Bearer <token>`.

Without `make` (for example on Windows without Git Bash tools), run the commands in the [Makefile](Makefile) directly.

## Configuration

All settings are environment variables. In development, a `.env` file is also read.

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | required | PostgreSQL connection string |
| `PUBLIC_BASE_URL` | required | Taskify's external address, used in notification action links |
| `AGENTPULSE_BASE_URL` | required | AgentPulse API |
| `AGENTPULSE_API_KEY` | required | Sent to AgentPulse as a bearer token |
| `AGENTPULSE_WEBHOOK_SECRET` | required | HMAC key for AgentPulse callbacks |
| `ENV` | `production` | `development` accepts identity headers and makes the JWT key optional |
| `PORT` | `8080` | HTTP port |
| `JWT_ALGORITHM` | `HS256` | `HS256` or `RS256` |
| `JWT_SECRET` / `JWT_PUBLIC_KEY` | required in production | Token verification key (PEM for RS256) |
| `TRUSTED_GATEWAY_CIDRS` | empty | Address ranges allowed to send identity headers instead of a token, judged by the TCP peer address |
| `OUTBOX_WORKERS` | `8` | Concurrent sends per replica |
| `OUTBOX_BATCH_SIZE` | `100` | Rows claimed at a time |
| `OUTBOX_POLL_INTERVAL` | `200ms` | Relay poll period |
| `OUTBOX_MAX_ATTEMPTS` | `10` | Attempts before a dead letter |
| `OUTBOX_LEASE` | `60s` | A claimed row not settled in this time is retried |
| `SCHEDULER_INTERVAL` | `1m` | Deadline scan period |
| `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST` | `20`, `40` | Per-user request rate, per replica |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | Export traces over OTLP/HTTP; other `OTEL_*` variables apply |
| `AGENTS_ENABLED` | `false` | Runs the agent runtime in this process. Workspaces still opt in through their agent settings. |
| `AGENT_WORKERS` | `4` | Concurrent agent jobs per replica |
| `AGENT_MAX_TURNS` | `6` | Model turns per job run |
| `AGENT_MAX_ATTEMPTS` | `3` | Failed runs before a job fails |
| `LLM_BASE_URL` | required with agents | OpenAI-compatible API root, such as `http://localhost:11434/v1` |
| `LLM_MODEL` | required with agents | Model name, such as `qwen2.5-coder:7b` |
| `LLM_API_KEY` | empty | Bearer token; leave empty for a local Ollama |
| `LLM_TIMEOUT` | `30s` | Limit for each model call |

A missing or invalid setting stops start-up with a message naming every problem.

## Agents and local models

Agents are off twice over by default: the process needs `AGENTS_ENABLED=true`, and each workspace needs `agents_enabled` in its agent settings (`PATCH /api/v1/workspaces/{id}/settings`). Even then every agent action waits for approval until the workspace also turns `require_hitl_for_all` off.

Agents talk to any OpenAI-compatible endpoint with tool calling. Locally that is [Ollama](https://ollama.com) with `qwen2.5-coder:7b` or `llama3.1:8b`, in either of two ways:

```bash
# 1. Ollama in Docker. The first start downloads the model (about 5 GB) into a volume.
AGENTS_ENABLED=true docker compose --profile ollama up -d --build

# 2. An Ollama already running on your machine, reused from the container.
ollama pull qwen2.5-coder:7b
AGENTS_ENABLED=true LLM_BASE_URL=http://host.docker.internal:11434/v1 docker compose up -d --build
```

Outside Docker, point `LLM_BASE_URL` at `http://localhost:11434/v1`. A hosted model works the same way with its own base URL, model name, and `LLM_API_KEY`; switching is configuration only.

A smoke test checks the client, the runtime, and the model together. It is skipped unless `LLM_BASE_URL` is set, so CI never needs a model:

```bash
LLM_BASE_URL=http://localhost:11434/v1 LLM_MODEL=qwen2.5-coder:7b go test ./tests/e2e -run TestLLMSmoke -v
```

## API

All routes are under `/api/v1` and need authentication, except the webhook. Field names are snake_case and timestamps are RFC 3339 in UTC.

| Method and path | Purpose |
| --- | --- |
| `POST /tasks` | Create a task in an existing workspace |
| `GET /tasks` | List: `assignee_id`, `status`, `priority`, `limit` (1 to 200), `cursor`. Archived tasks only with `status=archived` |
| `GET /tasks/{id}` | A task with its recent comments |
| `PATCH /tasks/{id}`, `PUT /tasks/{id}` | Update; needs the version in `If-Match` or the body |
| `POST /tasks/{id}/complete` | Mark done; optional `{"notification_id": …}` is reported to AgentPulse as acted |
| `DELETE /tasks/{id}` | Archive |
| `POST /tasks/{id}/restore` | Restore an archived task |
| `DELETE /tasks/{id}?permanent=true` | Delete for good, with its pending notifications (admin) |
| `POST /tasks/{id}/comments` | Comment; `@username` mentions are notified |
| `POST /tasks/{id}/feedback` | Forward `acted`, `dismissed`, `snoozed` (with `duration`), or `muted` to AgentPulse |
| `POST /workspaces` | Create a workspace and register its default AgentPulse rules (admin) |
| `GET`, `PATCH /workspaces/{id}/settings` | Read or rename a workspace (admin) |
| `POST /workspaces/{id}/bootstrap-rules` | Register missing default rules; safe to repeat (admin) |
| `GET /admin/learning/suggestions` | AgentPulse scoring suggestions (admin) |
| `POST /admin/learning/suggestions/{id}/decision` | `{"decision": "approve" \| "reject"}` (admin) |
| `POST /webhooks/agentpulse` | AgentPulse callbacks, authenticated by signature |

The original `/tasks/` routes still work and serve the same handlers.

**Concurrency.** Every task response carries `ETag: "<version>"`. An update without a version gets `428`; with a stale one, `409` and the current version in `error.details.current_version`.

**Errors** share one shape: `{"error": {"code": "…", "message": "…", "details": {…}}}`. Unparseable input is `400`, a rule violation `422`, a missing task (including one in another tenant) `404`.

**Webhook signing.** AgentPulse sends `X-AgentPulse-Timestamp` (Unix seconds) and `X-AgentPulse-Signature`, the hex HMAC-SHA256 of `<timestamp>.<raw body>` keyed with `AGENTPULSE_WEBHOOK_SECRET`. Timestamps more than 5 minutes from now are rejected, and a replayed callback changes nothing. The body names the `action` (`complete` or `snooze`), `tenantId`, `userId`, `taskId`, and optionally `notificationId` and `duration`.

## Operations

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | The process is up; reports its version |
| `GET /readyz` | `503` while PostgreSQL is unreachable |
| `GET /metrics` | Prometheus metrics; unauthenticated, so restrict it at the network edge |

| Metric | Labels |
| --- | --- |
| `taskify_http_request_duration_seconds` | `route`, `method`, `status` |
| `taskify_outbox_pending_rows` | none; queried at scrape time |
| `taskify_outbox_dispatch_duration_seconds` | `event_type`, `outcome` |
| `taskify_outbox_dlq_total` | `event_type` |
| `taskify_feedback_forwarded_total` | `signal` |

**Tracing.** Taskify continues an incoming W3C `traceparent` or starts a trace, stores it with each outbox row, and the relay restores it when calling AgentPulse, so one trace runs from the client's request to the notification. Logs are JSON and carry `request_id` and `trace_id`. Spans are exported when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.

**Shutdown.** On `SIGTERM` or interrupt, Taskify stops accepting requests, lets in-flight requests and sends finish, and exits. If it is killed outright, rows it had claimed are retried by another replica, or by the next start, once their lease expires; AgentPulse deduplicates any resend.

**Migrations** are not applied by the server. Apply them before starting a new version: `make migrate-up`, or the SQL files in `migrations/`, which also ship in every release archive and at `/migrations` in the container image.

## Development

```bash
go test ./...                       # unit tests; PostgreSQL tests are skipped
DATABASE_URL=postgres://taskify:taskify@localhost:5433/taskify?sslmode=disable \
  go test -race ./...               # everything, including PostgreSQL and end-to-end
```

- Tests that need PostgreSQL create and drop their own database, so they run in parallel and leave nothing behind. The role in `DATABASE_URL` needs permission to create databases.
- The race detector needs cgo. Where there is no C compiler, run the tests in a container: `docker run --rm --network taskify_default -e DATABASE_URL=postgres://taskify:taskify@postgres:5432/taskify?sslmode=disable -v "$PWD:/src" -w /src golang:1.26 go test -race ./...`
- CI runs vet, `golangci-lint`, the race tests, and a gate requiring 90% coverage in `domain`, `service`, `worker`, and `api`.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org); they decide the next version. Each merge to `main` is released automatically ([docs/releasing.md](docs/releasing.md)).

## Install a release

```bash
docker pull ghcr.io/rtrtarun/taskify:latest
```

Binaries for Linux, macOS, and Windows are attached to each [GitHub release](https://github.com/rtrtarun/taskify/releases).

Developed by Rongali Tulasi Ram Tarun.
