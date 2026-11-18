# Taskify Implementation Plan

This folder breaks the milestones in [milestones.md](../milestones.md) into tasks small enough to hand to one implementer (a person or a coding model) at a time. It contains no code.

| Document | Answers |
| --- | --- |
| [features.md](../features.md) | What we are building and why |
| [architecture.md](../architecture.md) | How it is designed. This is the authority for every task below. |
| [milestones.md](../milestones.md) | Delivery order, deliverables, and exit criteria per milestone |
| This folder | Who builds which piece, in what order, and how we know it is finished |
| [releasing.md](../releasing.md) | How merges to `main` are versioned, packaged, and released |

Task lists:

- [r1-task-service.md](r1-task-service.md): M0 to M4, 40 tasks
- [r2-agents.md](r2-agents.md): M5 to M11, 33 tasks
- [r3-clients.md](r3-clients.md): M12 to M15, 32 tasks

## 1. How the work is classified

### 1.1 By work stream

| Stream | Milestones | What it contains |
| --- | --- | --- |
| Foundation | M0 | Build, tooling, CI, mock AgentPulse, contract check |
| Data and domain | M1, M5 (schema part) | Migrations, Go types, state machine, events, keys |
| Persistence and delivery | M2 | Repositories, `TaskService`, outbox relay, scheduler, AgentPulse client |
| HTTP surface | M3, M6 (endpoints), M12 (list endpoints) | Router, auth, handlers, webhook, admin proxies |
| Operations | M4 | Health, metrics, tracing, shutdown, benchmarks |
| Agent engine | M5, M6 | Job queue, LLM client, tool registry, runtime, approval flow |
| Agents | M7 to M11 | Triage, decomposer, blocker, digest, learning |
| Contract | M12 | OpenAPI, schemas, fixtures, conformance runner |
| Clients | M13, M14, M15 | CLI, web frontend, Python SDK |

### 1.2 By who should do it

Every task carries one of three levels.

| Level | Meaning | Share |
| --- | --- | --- |
| **Routine** | Well specified, low risk. Hand to a smaller model or a junior engineer and review the diff normally. | 81 tasks |
| **Careful** | Concurrency, security, or transactional correctness. Can still be delegated, but the result needs review by a stronger model or a senior engineer before merge, and its tests must run with `-race`. | 24 tasks |
| **Owner** | Needs access, a decision, or an account only the project owner has. Cannot be delegated. | Part of 3 of the tasks above (M0-07, M14-03, M15-05), plus the confirmations in section 3 |

The Careful tasks are where a wrong implementation still passes a casual test: the outbox claim and relay, the scheduler, dependency cycle checks, the agent runtime, proposal apply, authentication, the webhook, and permanent delete.

## 2. Owner actions

These block specific tasks. Everything else can proceed without them.

| # | Action | Blocks | Until then |
| --- | --- | --- | --- |
| O1 | Give access to the AgentPulse repository or SDK so its contract can be read (task M0-07) | Final sign-off of M2, M3, M10, M11, M14 notification feed | Work proceeds against the mock AgentPulse built in M0-06. All AgentPulse field names stay inside `internal/agentpulse`, so a correction touches one package. |
| O2 | Choose the identity provider that issues JWTs (default G17) | `taskctl login` (M13-02), frontend sign-in (M14-03) | Development uses locally minted HS256 tokens from `make dev-token`. |
| O3 | Reserve `taskify-sdk` on the public Python index (decision 26) | Nothing | Not needed until v1.0. |

## 3. Defaults chosen where the architecture is silent

[architecture.md](../architecture.md) section 8.2 lists no open questions, and none of the 26 decisions is reopened here. While splitting the work, the points below turned out to have no stated answer. Each has a default so that an implementer does not invent one. **These are proposals by the author of this plan, not owner decisions.** Change the row here if a default is wrong; tasks refer to them by number.

| # | Gap | Default used in the tasks | Worth confirming |
| --- | --- | --- | --- |
| G1 | The relay returns rows stuck in `processing` after a lease, but the outbox table has no column recording when a row was claimed | Add `claimed_at TIMESTAMPTZ` to `notification_outbox` in migration `000002`. Lease is 60 seconds (`OUTBOX_LEASE`). | |
| G2 | One `last_due_soon_notified_at` column serves two reminder windows; a rescheduled task must be reminded again | 24 h reminder fires when the column is null. 1 h reminder fires when the column is earlier than `due_at - 1h`. Changing `due_at` clears the column. Keys become `...:due_soon_24h:{due_at as Unix seconds}` and `...:due_soon_1h:{...}`. | |
| G3 | Admin-only endpoints exist, but where the role comes from is not stated | JWT claim `role` with values `admin` or `member`. In the development header fallback, `X-User-Role`. | Yes |
| G4 | Error mapping says "400 or 422" | Unparseable request: `400`. `ErrValidation` and `ErrInvalidStateTransition`: `422`. Authenticated but not allowed: `403`. | |
| G5 | Rate limit has no numbers | Token bucket per tenant and user, in memory per replica. `RATE_LIMIT_RPS=20`, `RATE_LIMIT_BURST=40`. | |
| G6 | Watchers have a table but no endpoint | `watcher_ids` accepted on task create and update. No separate watcher endpoints. | |
| G7 | Webhook signature layout and action names are not given | Header `X-AgentPulse-Timestamp`; signature is hex HMAC-SHA256 of `{timestamp}.{raw body}`. Actions: `complete`, `snooze`, `unblock`, `approve`, `reject`. | Verified in M0-07 |
| G8 | What "Snooze 2h" changes in Taskify | Sets `remind_at` to now plus the duration and forwards `snoozed` feedback. No task event. | Yes |
| G9 | `POST /workspaces` registers AgentPulse rules; behaviour when AgentPulse is down is not stated | The workspace is created anyway and the response carries `rules_bootstrapped: false`. `bootstrap-rules` is idempotent and can be re-run. | |
| G10 | Who may approve or reject a proposal | The task's assignee, its creator, or an admin. Anyone else gets `403`. | Yes |
| G11 | Agent jobs have no crash recovery or fixed limits | A job `running` with `updated_at` older than 5 minutes returns to `pending` with `attempts + 1`. Maximum 3 attempts, 6 model turns per run (`AGENT_MAX_ATTEMPTS`, `AGENT_MAX_TURNS`). | |
| G12 | Length of a "stale period" | 48 hours without a comment (given); one investigation per task per calendar day. | |
| G13 | The blocker escalation notification has no event type | Sent as `task.blocked` with `payload.escalation: true` to the assignee of the blocking task (creator if unassigned). Key suffix `:escalation:{job_id}`. | Yes |
| G14 | The digest gatekeeper is listed as an agent, but the notification row is written in the task transaction, before any agent could run | A deterministic count inside the task transaction, with no model call. Threshold 3 (`DIGEST_LOAD_THRESHOLD`). Skipped when the `digest` agent is disabled for the workspace. | Yes |
| G15 | Morning brief time, event type, and the endpoint the frontend reads | 08:00 UTC (`DIGEST_BRIEF_HOUR_UTC`); no per-user time zone. Event type `digest.morning_brief`. Frontend reads `GET /api/v1/me/brief`. | Yes |
| G16 | The learning agent "reads AgentPulse feedback analytics", but no endpoint is named | Endpoint identified in M0-07. The job runs weekly. | Verified in M0-07 |
| G17 | No identity provider is named and Taskify has no login endpoint | Owner action O2. Development uses `make dev-token`. | Owner |
| G18 | How triage finds "similar open tasks" | PostgreSQL full-text match on the title, same workspace, open tasks only, top 5. No embeddings. | |
| G19 | Libraries not named in the architecture | `pgx` v5, `golang-migrate`, `golangci-lint`, `prometheus/client_golang`, `golang-jwt/jwt` v5, `santhosh-tekuri/jsonschema`, `oapi-codegen`, `kin-openapi`, `openapi-typescript`, Playwright. | |
| G20 | A failed feedback forward after a notification ack is "retried through the outbox", but outbox rows are `/notify` calls | Outbox rows with `event_type = 'feedback.forward'` are sent by the relay to `/feedback`. | |
| G21 | Status code when decomposition is requested for a disabled agent | `409` with error code `agent_disabled`. | |
| G22 | The owner's machine is Windows; the plan calls for a `Makefile` | Targets are plain commands that run under Git Bash. | |

## 4. Order of work

The dependency graph is in [milestones.md](../milestones.md). Within a milestone, the **After** column of each task gives its prerequisites.

| Phase | Milestones | Can run in parallel |
| --- | --- | --- |
| 1 | M0 | M0-07 runs alongside everything once access exists |
| 2 | M1 → M2 → M3 → M4 | Within M2: the Postgres repositories, memory repositories, and AgentPulse client are independent |
| 3 | M5 → M6 | M12 can start here, since it only needs R1 |
| 4 | M7, M8, M9 | All three are independent of each other |
| 5 | M10, M11 | Independent of each other |
| 6 | M13, M14a, M15 after M12 | All three are independent; M14b needs M6, M8, M10 |

One milestone is finished only when every exit criterion in [milestones.md](../milestones.md) passes, not when its task list is ticked off.

## 5. How to hand a task to an implementer

Give the implementer one task ID at a time, or a few tasks from the same milestone that touch the same package.

Brief template:

```text
Implement task <ID> from docs/implementation/<file>.md in the taskify repository.

Read first:
- the task row, and the rows it lists under After
- the sections of docs/architecture.md named in the task
- docs/implementation/README.md sections 3 and 6

Stay inside the files the task names. Write the tests the task lists.
Finish by running: go build ./... && go vet ./... && go test -race ./...
Report: what you built, test output, and anything in the architecture that
was unclear or contradictory. Do not resolve a contradiction by guessing.
```

For a Careful task, add: "Explain how the code behaves when two replicas run this at the same time."

## 6. Rules for every task

1. **The architecture wins.** If a task row and [architecture.md](../architecture.md) disagree, follow the architecture and report the disagreement.
2. **No remote calls on a task write path.** Handlers that change tasks write to PostgreSQL only. The few endpoints that exist to talk to AgentPulse use a timeout and the circuit breaker.
3. **One transaction per state change.** Task row, outbox rows, and agent job rows commit together.
4. **Tenant comes from the request context**, never from a request body or from model output. Every query filters on it. Another tenant's row is `404`.
5. **Dependency direction:** `api` → `service` → `domain`. `domain` imports nothing from the project.
6. **Tests come with the code.** Table-driven where there is a matrix. `go test -race ./...` passes before a task is reported as done. Tests that need PostgreSQL are skipped with a clear message when `DATABASE_URL` is unset.
7. **Migrations have a working down script.**
8. **Secrets are never logged**, including in tests and error messages.
9. **AgentPulse payloads are camelCase; the Taskify API is snake_case.**
10. **Commit messages follow Conventional Commits** (`feat:`, `fix:`, `docs:`, ...): they decide the next release version. See [releasing.md](../releasing.md).
11. **From M12 onward**, a change to an endpoint, event, or tool is made in `contract/` first, and generated clients are regenerated in the same change.
12. **Do not widen scope.** A missing prerequisite is reported, not built ad hoc.

## 7. Progress

Update this table as milestones close.

| Milestone | Tasks | Status |
| --- | --- | --- |
| M0 Foundation | 7 | Done except M0-07, which waits on AgentPulse access (O1) |
| M1 Domain and schema | 7 | Done. Migrations verified up, down, up on PostgreSQL 16 |
| M2 Outbox and workers | 10 | Done. All exit criteria have tests, passing with `-race` on memory and PostgreSQL |
| M3 REST API and bridges | 10 | Done. Unit, API, and end-to-end tests pass with `-race` on PostgreSQL |
| M4 Observability (R1) | 6 | Done. Benchmarks meet every target on a Linux network; see docs/benchmarks.md. R1 complete apart from M0-07 |
| M5 Agent runtime | 11 | Not started |
| M6 HITL bridge | 4 | Not started |
| M7 Triage | 4 | Not started |
| M8 Decomposer | 4 | Not started |
| M9 Blocker | 4 | Not started |
| M10 Digest | 3 | Not started |
| M11 Learning (R2) | 3 | Not started |
| M12 Contract and conformance | 8 | Not started |
| M13 CLI | 6 | Not started |
| M14 Web frontend (R3) | 13 | Not started |
| M15 Python SDK | 5 | Not started |

## 8. Departures found during implementation

Recorded as they come up, so the architecture can be corrected or the code changed.

| # | Where | What the code does | Why |
| --- | --- | --- | --- |
| D1 | M1-05 | Creating a task raises `task.created` only, not `task.created` and `task.assigned` as the endpoint table in section 5.4 suggests. | Both would notify the same assignee about the same action. |
| D2 | M1-06 | The key for `task.comment_added` uses the comment ID instead of `v{version}`. | A comment does not change the task version, so two comments would share a key and the second would be dropped. |
| D3 | M1-02 | `notification_outbox.idempotency_key` is `VARCHAR(320)`, not 192. | A key holding a tenant ID, task ID, event type, and recipient ID can exceed 192 characters. |
| D4 | M0 | `go.mod` requires Go 1.26. | Current versions of `pgx` and `golang.org/x/sync` require it. |
| D5 | M2-08 | Outbox rows carry `claimed_at`, and every result update is fenced on it. | A worker whose lease expired must not overwrite the result of the worker that reclaimed the row. |
| D6 | M2-09 | Deadline reminders do not change a task's `version` or `updated_at`. | A reminder is not an edit; bumping the version would make a client's next save fail with `409`. |
| D7 | M2-02 | `GET /tasks?status=archived` lists archived tasks; any other list excludes them. | Otherwise there is no way to find an archived task in order to restore it. |
| D8 | M3-06 | New migration `000003_integrations` (`workspace_rules`, `webhook_deliveries`). The planned agent schema becomes `000004` and notification acks `000005`. | Bootstrapping rules must be repeatable without duplicates, including after a partial failure, and a replayed webhook must change nothing; both need a record. |
| D9 | M3-06 | Default rules use a `matchAny` list of conditions, and the immediate-escalation rule matches manual blocks only. | The source rules need "A or B" conditions, and decision 23 makes dependency blocks quiet. The rule shape is unverified (M0-07). |
| D10 | M3-08 | A replayed webhook is recognised by its signature; there is no delivery ID. | A byte-identical replay has the same signature, and AgentPulse's payload has no confirmed ID field. |
| D11 | M3-03 | An update without `If-Match` or `version` is `428 Precondition Required`; `PUT` behaves as `PATCH`. | The architecture requires a version but names no status; 428 is the standard one. Section 5.4 lists `PUT` and `PATCH` as one row. |
| D12 | M3-05 | Permanent delete returns `204`; `?permanent=` accepts only `true` or `false`. | Nothing remains to return, and an unexpected value must not fall through to an archive. |
| D13 | M4-02 | Traces are exported only when an OTLP endpoint is configured. Without one, spans are still created, so trace IDs still reach logs, the outbox, and AgentPulse. | Tracing must not need a collector to be useful, and must not fail start-up when none exists. |
| D14 | M4-01 | `/metrics` and `/readyz` are unauthenticated, like `/healthz`. | Prometheus and orchestrator probes do not carry tokens; access is restricted at the network edge, as the README says. |
