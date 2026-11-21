# Taskify

[![CI](https://github.com/rtrtarun/taskify/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/rtrtarun/taskify/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/rtrtarun/taskify?sort=semver)](https://github.com/rtrtarun/taskify/releases/latest)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)
[![Container](https://img.shields.io/badge/container-ghcr.io-2496ED?logo=docker&logoColor=white)](https://github.com/rtrtarun/taskify/pkgs/container/taskify)
![OpenTelemetry](https://img.shields.io/badge/OpenTelemetry-traces-425CC7?logo=opentelemetry&logoColor=white)
![Prometheus](https://img.shields.io/badge/Prometheus-metrics-E6522C?logo=prometheus&logoColor=white)
![Ollama](https://img.shields.io/badge/LLM-Ollama%20or%20OpenAI--compatible-000000?logo=ollama&logoColor=white)
![Coverage gate](https://img.shields.io/badge/coverage%20gate-90%25-brightgreen)
[![License](https://img.shields.io/badge/license-MIT%20OR%20Apache--2.0-blue)](#license)

> **Tasks change all day. People should hear about the ones that matter.** Taskify is a multi-tenant task service that hands every notification decision to **AgentPulse**, and runs optional AI agents that tidy tasks while a person stays in control.

Status: **v0.5.0** (2026-11-21). Release R1, the attention-aware task service, is complete (milestones M0 to M4). Of R2, the agentic plane, the agent runtime, human approval, and the triage agent are built (M5 to M7); the decomposer, blocker, digest, and learning agents come next. The CLI, web frontend, and Python SDK (R3) have not started. The AgentPulse contract has so far been checked only against a mock: [what is verified and what is not](#roadmap-and-known-issues) is written down, not hidden.

---

## Table of contents

1. [About the project](#about-the-project)
2. [Key features](#key-features)
3. [Tech stack](#tech-stack)
4. [Project structure](#project-structure)
5. [Getting started](#getting-started)
6. [Usage examples](#usage-examples)
7. [Agents and local models](#agents-and-local-models)
8. [API](#api)
9. [Operations](#operations)
10. [Testing](#testing)
11. [Documentation map](#documentation-map)
12. [Roadmap and known issues](#roadmap-and-known-issues)
13. [Contributing](#contributing)
14. [License](#license)
15. [Acknowledgements and support](#acknowledgements-and-support)

---

## About the project

### What it does

Taskify records what happens to a task; AgentPulse decides who is told, when, and on which channel, so people get one alert for what matters instead of twenty for everything. Task changes are written with their notifications in a single PostgreSQL transaction (a transactional outbox), and a background relay delivers them. The API never waits on AgentPulse, and an AgentPulse outage delays notifications without losing them.

Optional AI agents work on tasks in the background. The triage agent labels, describes, estimates, and links new tasks, and proposes priority changes for a person to approve.

```mermaid
flowchart LR
    client["Client: CLI, web, scripts"] -->|"REST /api/v1"| api["HTTP API"]
    api -->|"one transaction"| pg[("PostgreSQL: tasks, outbox, agent jobs")]
    sched["Deadline scheduler"] --> pg
    relay["Outbox relay"] -->|"claim rows"| pg
    relay -->|"/notify"| ap["AgentPulse"]
    ap -->|"Slack, email, in-app"| user["People"]
    user -->|"Mark done, Snooze, Approve"| ap
    ap -->|"signed webhook"| api
    runtime["Agent runtime"] -->|"claim jobs"| pg
    runtime -->|"tool calls"| llm["Model: Ollama or hosted"]
```

- **Write path:** a request changes a task and inserts one outbox row per notification in the same transaction.
- **Relay:** claims rows with `FOR UPDATE SKIP LOCKED`, sends them, and retries with backoff; after 10 failures a row is parked as a dead letter. Each notification carries a deterministic idempotency key, so a resend is never delivered twice.
- **Scheduler:** writes `task.due_soon` and `task.overdue` reminders. Any number of replicas can run.
- **Webhook:** AgentPulse calls back when someone acts on a notification, and Taskify applies the action.
- **Agents:** a person's change queues agent jobs in the same transaction. The agent runtime runs each job with a model, and every action the model asks for is checked by a tool registry. Changes that need approval wait as proposals, decided through the API or a notification.

**The rule that never bends:** the model never touches the database. It can only ask for a registered tool, and typed Go code decides whether and how that request runs. Tenant and task scope come from the job row, never from model output, and a change made by an agent never starts more agent work.

### Why it is useful

Most task managers notify everyone about everything, and people learn to ignore them. Taskify emits one well-described event per change and lets AgentPulse apply budgets, digests, and learned preferences, so an urgent blocker still interrupts while a label change waits for the digest. The agents take the clerical work (labels, estimates, dependencies) off people, but anything consequential, such as a priority change, waits for someone to approve it.

---

## Key features

**Tasks**
- Tenant-scoped tasks with status, priority, assignee, watchers, labels, comments with `@mentions`, due and reminder times, and optimistic concurrency through `version` and `ETag`
- Dependencies between tasks with cycle checks; a dependency-blocked task is released when its last blocker is completed or archived
- Soft archive and restore, and a permanent delete that also removes pending notifications

**Notify**
- A transactional outbox: no lost or duplicated notifications, even across crashes and AgentPulse outages
- Deadline reminders (24 hours and 1 hour before, then overdue) from a scheduler safe to run on every replica
- Default AgentPulse routing rules per workspace, feedback forwarding (`acted`, `dismissed`, `snoozed`, `muted`), and signed webhooks for "Mark done" and "Snooze"

**Agents**
- A job queue written in the task transaction, deduplicated per task version, with leases, retry backoff, and crash recovery
- An OpenAI-compatible model client (Ollama, vLLM, hosted models) with tool calling, timeouts, and backoff
- A tool registry that rejects unknown tools, invalid arguments, and tasks outside the job's scope, using a standard-library JSON Schema validator
- Human-in-the-loop approval: proposals wait for the assignee, the creator, or an admin, who can approve or reject from the API or the notification itself
- The **triage agent**: labels from the workspace's vocabulary, descriptions, estimates, dependency links, and priority recommendations
- Off twice over by default: per process (`AGENTS_ENABLED`) and per workspace, with approval required for every action until an admin relaxes it

**Operate**
- Health and readiness endpoints, Prometheus metrics, JSON logs with request and trace IDs, and OpenTelemetry traces from the request through the outbox to AgentPulse and through each agent run
- Graceful shutdown; any number of replicas coordinating only through PostgreSQL
- Automatic semantic-versioned releases: binaries for Linux, macOS, and Windows, and a multi-arch container image

---

## Tech stack

| Area | Technology |
| --- | --- |
| Service | Go 1.26, `chi` router, `pgx` v5 |
| Database | PostgreSQL 16, plain SQL migrations (`golang-migrate`) |
| Notifications | AgentPulse (`agentic-notification`) over HTTP, with a mock for tests |
| Agents | OpenAI-compatible chat completions with tool calling; Ollama (`qwen2.5-coder:7b`, `llama3.1:8b`) locally |
| Auth | JWT (HS256 or RS256), HMAC-signed webhooks |
| Observability | Prometheus `client_golang`, OpenTelemetry tracing (OTLP/HTTP), `log/slog` JSON logs |
| Tests | `go test -race` against PostgreSQL, a 90% coverage gate, `golangci-lint` |
| Ops | Docker, Docker Compose, GitHub Actions, GoReleaser, GitHub Container Registry |

Dependencies are deliberately few. Tool argument validation uses only the standard library.

---

## Project structure

```text
.
├── cmd/
│   ├── taskify/              The server
│   ├── mock-agentpulse/      A recording AgentPulse stand-in for local runs and tests
│   └── devtoken/             Prints a signed development JWT
├── internal/
│   ├── domain/               Tasks, events, dependencies, agent jobs: rules with no I/O
│   ├── service/              Use cases; one transaction per state change
│   ├── repository/           Interfaces, PostgreSQL and in-memory stores, shared conformance tests
│   ├── api/                  HTTP handlers, auth, webhook, error mapping
│   ├── worker/               Outbox relay, deadline scheduler, cleanup
│   ├── agent/                Agent runtime and the triage agent
│   │   ├── llm/              Model client and scripted mock
│   │   └── tools/            Tool registry, schema validator, UpdateTaskAttributes, LinkDependencies
│   ├── agentpulse/           AgentPulse client and circuit breaker
│   ├── observability/        Metrics and tracing
│   ├── config/               Environment configuration
│   └── app/                  Wiring shared by the server and end-to-end tests
├── migrations/               SQL migrations, shipped with every release
├── tests/                    End-to-end tests, benchmarks, mock AgentPulse
├── tools/nextversion/        Picks the next version from commit messages
└── docs/                     Features, architecture, milestones, plans, releasing
```

---

## Getting started

### Prerequisites

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | 1.26 | Building and testing |
| Docker with Compose | any recent | PostgreSQL, the mock AgentPulse, and optionally Ollama |
| `make` | optional | Shortcuts; without it, run the commands in the [Makefile](Makefile) directly |

### Install

```bash
git clone https://github.com/rtrtarun/taskify.git
cd taskify
```

Or run a release without building:

```bash
docker pull ghcr.io/rtrtarun/taskify:latest
```

Binaries for Linux, macOS, and Windows are attached to each [GitHub release](https://github.com/rtrtarun/taskify/releases).

### Run locally

```bash
docker compose up -d --build                  # PostgreSQL, a mock AgentPulse, Taskify
make migrate-up                               # apply migrations to the compose database
curl localhost:8080/healthz                   # {"status":"ok","version":"dev"}
```

The compose database is published on host port **5433**, so it does not collide with a local PostgreSQL on 5432. The mock AgentPulse on port 9090 records every call; `GET localhost:9090/_mock/requests` shows them.

### Configure

All settings are environment variables. In development, a `.env` file is also read. A missing or invalid setting stops start-up with a message naming every problem.

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

---

## Usage examples

Compose runs Taskify with `ENV=development`, which accepts identity headers instead of a token:

```bash
H='-H X-Tenant-ID:tenant_dev -H X-User-ID:usr_admin -H X-User-Role:admin -H Content-Type:application/json'

# A workspace, then a task in it.
curl -X POST localhost:8080/api/v1/workspaces $H -d '{"id":"ws_core","name":"Core"}'
curl -X POST localhost:8080/api/v1/tasks $H \
  -d '{"workspace_id":"ws_core","title":"Fix checkout crash","priority":"p1_critical","assignee_id":"usr_bob"}'
curl localhost:8080/api/v1/tasks $H

# Turn agents on for the workspace, with approval still required for every action.
curl -X PATCH localhost:8080/api/v1/workspaces/ws_core/settings $H \
  -d '{"agent_settings":{"agents_enabled":true}}'

# Read a proposal and approve it.
curl localhost:8080/api/v1/agent-jobs/<job-id> $H
curl -X POST localhost:8080/api/v1/agent-jobs/<job-id>/apply $H
```

To use a token instead, `make dev-token ARGS="-tenant tenant_dev -user usr_bob -role member"` prints a signed development JWT for `Authorization: Bearer <token>`.

---

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

| Agent | Runs when | May do at once | Needs approval |
| --- | --- | --- | --- |
| Triage | A person creates or edits a task | Labels, description, estimate, dependency links | Priority changes |

---

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
| `GET`, `PATCH /workspaces/{id}/settings` | Read or change a workspace's name and `agent_settings` (admin) |
| `POST /workspaces/{id}/bootstrap-rules` | Register missing default rules; safe to repeat (admin) |
| `GET /admin/learning/suggestions` | AgentPulse scoring suggestions (admin) |
| `POST /admin/learning/suggestions/{id}/decision` | `{"decision": "approve" \| "reject"}` (admin) |
| `GET /agent-jobs/{id}` | An agent job: status, proposed actions, result |
| `POST /agent-jobs/{id}/apply` | Approve a job's proposals (the task's assignee, its creator, or an admin). `409` if the task changed since (`stale_version`) or the job was decided the other way |
| `POST /agent-jobs/{id}/dismiss` | Reject a job's proposals; nothing changes |
| `POST /webhooks/agentpulse` | AgentPulse callbacks, authenticated by signature |

The original `/tasks/` routes still work and serve the same handlers.

**Concurrency.** Every task response carries `ETag: "<version>"`. An update without a version gets `428`; with a stale one, `409` and the current version in `error.details.current_version`.

**Errors** share one shape: `{"error": {"code": "…", "message": "…", "details": {…}}}`. Unparseable input is `400`, a rule violation `422`, a missing task (including one in another tenant) `404`.

**Webhook signing.** AgentPulse sends `X-AgentPulse-Timestamp` (Unix seconds) and `X-AgentPulse-Signature`, the hex HMAC-SHA256 of `<timestamp>.<raw body>` keyed with `AGENTPULSE_WEBHOOK_SECRET`. Timestamps more than 5 minutes from now are rejected, and a replayed callback changes nothing. The body names the `action` (`complete`, `snooze`, `approve`, or `reject`), `tenantId`, `userId`, the `taskId` (complete, snooze) or `jobId` (approve, reject), and optionally `notificationId` and `duration`.

---

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
| `taskify_agent_jobs_skipped_total` | `agent_type`, `reason` |
| `taskify_agent_job_duration_seconds` | `agent_type`, `outcome` |
| `taskify_agent_llm_tokens_total` | `agent_type`, `direction` |
| `taskify_agent_proposals_total` | `agent_type`, `decision` |

**Tracing.** Taskify continues an incoming W3C `traceparent` or starts a trace, stores it with each outbox row and agent job, and restores it when calling AgentPulse or running the job, so one trace runs from the client's request to the notification and through the agent's model calls. Logs are JSON and carry `request_id` and `trace_id`. Spans are exported when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.

**Shutdown.** On `SIGTERM` or interrupt, Taskify stops accepting requests, lets in-flight requests and sends finish, returns running agent jobs to the queue, and exits. If it is killed outright, claimed outbox rows and agent jobs are picked up again once their lease expires; AgentPulse deduplicates any resend.

**Migrations** are not applied by the server. Apply them before starting a new version: `make migrate-up`, or the SQL files in `migrations/`, which also ship in every release archive and at `/migrations` in the container image.

**Releases** are automatic: every merge to `main` that passes CI is versioned from its Conventional Commit messages, tagged, and published. See [docs/releasing.md](docs/releasing.md).

---

## Testing

```bash
go test ./...                       # unit tests; PostgreSQL tests are skipped
DATABASE_URL=postgres://taskify:taskify@localhost:5433/taskify?sslmode=disable \
  go test -race ./...               # everything, including PostgreSQL and end-to-end

# Optional: the agent stack against a real model.
LLM_BASE_URL=http://localhost:11434/v1 LLM_MODEL=qwen2.5-coder:7b go test ./tests/e2e -run TestLLMSmoke -v
```

- Tests that need PostgreSQL create and drop their own database, so they run in parallel and leave nothing behind. The role in `DATABASE_URL` needs permission to create databases.
- Every repository behaviour runs against both the in-memory store and PostgreSQL through one conformance suite.
- The race detector needs cgo. Where there is no C compiler, run the tests in a container: `docker run --rm --network taskify_default -e DATABASE_URL=postgres://taskify:taskify@postgres:5432/taskify?sslmode=disable -v "$PWD:/src" -w /src golang:1.26 go test -race ./...`
- CI runs vet, `golangci-lint`, the race tests, and a gate requiring 90% coverage in `domain`, `service`, `worker`, and `api`. No test in CI needs a model: agents are tested with a scripted mock.

---

## Documentation map

| Document | Covers |
| --- | --- |
| [docs/features.md](docs/features.md) | What Taskify does and why |
| [docs/architecture.md](docs/architecture.md) | How it is built: flows, data model, state machines, decisions |
| [docs/milestones.md](docs/milestones.md) | Delivery order, exit criteria, and status per milestone |
| [docs/implementation/](docs/implementation/README.md) | Task-level plans, rules for contributors, and every departure from the architecture |
| [docs/releasing.md](docs/releasing.md) | Versioning and releases |
| [docs/benchmarks.md](docs/benchmarks.md) | Measured latency |
| [docs/archive/](docs/archive/README.md) | Finished plans and the original proposals |

---

## Roadmap and known issues

**Next** (details in [milestones.md](docs/milestones.md)):

1. **More agents** (M8 to M11): the decomposer splits a task into subtasks on request, the blocker agent chases stalled work, the digest agent writes a morning brief, and the learning agent proposes notification rules from feedback.
2. **Contract and conformance** (M12): an OpenAPI document, event and tool schemas, and a black-box conformance suite.
3. **Clients** (M13 to M15): the `taskctl` CLI, a web frontend, and a Python client SDK, all from the same contract.

**Known issues**

- **The AgentPulse contract is unverified.** Request shapes, rule format, and webhook layout come from design notes and have only run against the mock AgentPulse in this repository (task M0-07, waiting on access).
- **The agents have not run against a real model in CI.** `TestLLMSmoke` exercises one; run it locally before relying on a particular model.
- There is no endpoint for a person to add a dependency yet; agents and the service can. It arrives with the API contract (M12).
- Approving from a notification decides as an ordinary member: AgentPulse does not send a role, so an admin who is neither assignee nor creator must use the API.
- No identity provider is chosen; development uses locally signed tokens.

---

## Contributing

Contributions are welcome. The short version, with the full rules in [docs/implementation/README.md](docs/implementation/README.md):

1. Branch from `main` (`feature/<topic>` or `fix/<topic>`).
2. [docs/architecture.md](docs/architecture.md) is the authority. Record any departure from it in section 8 of the implementation README.
3. One transaction per state change; the tenant always comes from the request context; dependencies point api → service → domain.
4. Add tests and run `go test -race ./...` against PostgreSQL; keep the coverage gate green.
5. Open a pull request describing what changed, what you ran, and anything you could not verify.

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org), because they decide the next version, and carry no `Co-Authored-By` trailer.

---

## License

Taskify is dual-licensed under either of

- the [MIT License](LICENSE-MIT), or
- the [Apache License, Version 2.0](LICENSE-APACHE),

at your option. Apache 2.0 adds an explicit patent grant; MIT is shorter. Both licence files ship in every release archive.

Unless you say otherwise, any contribution you submit for inclusion in Taskify is dual-licensed in the same way, without any additional terms or conditions.

---

## Acknowledgements and support

**Built on** Go, `chi`, `pgx`, `golang-jwt`, the Prometheus and OpenTelemetry Go libraries, GoReleaser, and Ollama. **Works with** [AgentPulse](https://github.com/rtrtarun/agentic-notification), which decides how and when people are notified.

**Help**

- Questions and bugs: [open an issue](https://github.com/rtrtarun/taskify/issues).
- Something unclear in the docs: start at the [documentation map](#documentation-map).
- Security problems: please open a private security advisory on the repository rather than a public issue.

Developed by Rongali Tulasi Ram Tarun.
