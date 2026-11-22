# R3 Tasks: Clients and Portability (M12 to M15)

Read [README.md](README.md) first: it explains the levels (Routine, Careful, Owner), the defaults G1 to G22, and the rules every task follows. Section numbers below refer to [architecture.md](../architecture.md), mostly section 9. Exit criteria for each milestone are in [milestones.md](../milestones.md).

M12 needs only R1, so R3 can run alongside R2. Tasks that need an R2 milestone say so in the **After** column.

Two rules apply to every task in this file:

- Clients contain no business rules: no transition checks, no key building, no tier logic. Those stay in the server.
- Generated code is never edited by hand. CI regenerates it and fails on a difference.

---

## M12. Contract and conformance suite

Goal: the API is written down once, every client is generated from it, and the server is checked against it (feature C1).

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M12-01 | **OpenAPI document.** Every endpoint in section 5.4 that exists so far, with request and response schemas, the shared error body, status codes, and the `If-Match` header. Field names are snake_case; timestamps are RFC 3339 in UTC. Written from the running server's behaviour, not from memory. | `contract/openapi.yaml` | The file passes an OpenAPI linter and covers every route the router registers (checked by a test that compares the two lists) | Routine, L | R1 |
| M12-02 | **Event and tool schemas.** One JSON Schema per event payload and per agent tool. The tool registry loads its schemas from these files at start-up, so a tool has one definition. | `contract/events/*.schema.json`, `contract/tools/*.schema.json`, `contract/embed.go`, `internal/agent/tools/registry.go` | Tests: each event the server emits validates against its schema; the registry fails to start with a missing tool schema | Routine, M | M12-01; tool part needs M5-08 |
| M12-03 | **Rules document.** The rules a schema cannot express: the transition matrix, block reasons, idempotency key formats, safety tiers, timestamp and UUID formatting, and the client rules in section 9.2. | `contract/RULES.md` | Each rule cites the fixture that encodes it, once M12-07 exists | Routine, S | M12-01 |
| M12-04 | **List endpoints for clients.** `GET /agent-jobs` with filters, `GET /tasks/{id}/subtasks`, `GET /tasks/{id}/agent-runs`, `GET /admin/outbox` with a status filter, `POST /admin/outbox/{id}/replay` (dead letter back to `pending`), `GET /me`, and the `id_prefix` filter on `GET /tasks`. Added to the contract first, then implemented. | `contract/openapi.yaml`, `internal/api/`, repositories, tests | Handler tests per endpoint; admin routes give `403` to members; a replayed row is dispatched | Routine, M | M12-01; agent endpoints need M6 |
| M12-05 | **Response validation in CI.** Handler and end-to-end tests validate every response against `openapi.yaml`. | `internal/api/` test helpers, CI | CI fails when a handler returns a body or status the contract does not describe | Routine, M | M12-01 |
| M12-06 | **Conformance runner.** A black-box runner that reads fixtures and executes them against `TARGET_URL` (section 9.3). It starts the mock AgentPulse and a scriptable mock LLM HTTP server, signs webhooks, and asserts outbox effects through `GET /admin/outbox` and through what the mock received. It imports no server packages and never reads the database. Define the fixture file format here. | `tests/conformance/`, `cmd/mock-llm/`, `contract/fixtures/README.md` | `TARGET_URL=... go test ./tests/conformance/...` runs a sample fixture of each kind against the Go server | Careful, L | M12-04 |
| M12-07 | **Fixtures.** The groups in section 9.3: task API, every cell of the transition matrix, events with keys and recipients, delivery (503 then 200; ten failures), webhooks, authentication, and, once R2 exists, agents and HITL. | `contract/fixtures/` | All fixtures pass; a test confirms every matrix cell has one | Routine, L | M12-06 |
| M12-08 | **R2 gap checklist.** If M12 finishes before R2, list the agent endpoints, events, and tools still missing from the contract, and tick them off as M5 to M11 land. | `contract/TODO.md` | The list is empty when R2 is complete | Routine, S | M12-07 |

From this milestone on, rule 10 in [README.md](README.md) applies: contract first, then code, then regenerated clients.

---

## M13. CLI `taskctl`

Goal: everything a user or operator does often can be done from a terminal (feature C3). Design in section 9.5.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M13-01 | **Generated Go client.** Generate `pkg/client` from `openapi.yaml`, with a `make generate` target and a CI check that regeneration produces no difference. | `pkg/client/`, `Makefile`, CI | A changed contract without regeneration fails CI | Routine, S | M12-05 |
| M13-02 | **CLI skeleton, configuration, credentials.** Cobra root command; Viper configuration with named profiles; precedence flag, then environment variable, then profile; `login` and the `config` subcommands. Credentials go to the operating system keychain where available, otherwise to a file readable only by the user, and are never printed. The exit codes in section 9.5. `login` against a real identity provider waits on owner action O2; until then it stores a pasted token. | `cmd/taskctl/` | Tests: precedence order; each exit code; no credential appears in output with `--verbose` | Careful, M | M13-01 |
| M13-03 | **Task commands.** `task add`, `list`, `show`, `update`, `done`, `block`, `unblock`, `archive`, `comment`, `decompose`. `update` reads the task, applies the change, and sends `If-Match`; on `409` it reports the current version and exits with code 5 without retrying. IDs may be a unique prefix. | `cmd/taskctl/task*.go` | Tests against a local server cover each command and the conflict path | Routine, M | M13-02 |
| M13-04 | **Job and admin commands.** `job list`, `show`, `approve`, `reject`; `admin suggestions`, `rules bootstrap`, `outbox list`, `outbox replay`. | `cmd/taskctl/job*.go`, `admin*.go` | Tests: `job approve` applies a Tier 2 proposal; a member running an admin command exits with code 3 | Routine, M | M13-02; job commands need M6 |
| M13-05 | **Output and ergonomics.** Aligned tables by default, `--json` printing the API body unchanged, `--quiet` printing IDs only, `--watch` on `job show`, `--wait` on `decompose`, shell completion for bash, zsh, and PowerShell, `version`, and the CLI version in `User-Agent`. | `cmd/taskctl/output.go` and others | Tests: `--json` output parses and matches the API body | Routine, S | M13-03 |
| M13-06 | **Scripted session and release builds.** An automated session that creates a task, comments, completes it, and reads it back with `--json`. Cross-compiled builds for Windows, macOS, and Linux. | `tests/cli/`, `Makefile` or release configuration | The session passes in CI; three binaries are produced | Routine, M | M13-05 |

---

## M14. Web frontend

Goal: someone who has never seen the API can manage tasks and supervise agents in a browser (feature C4). Design in section 9.6.

The stack is React, TypeScript, Vite, TanStack Query, and React Router. Task and comment text is always rendered as text, never as HTML, including text written by agents.

### M14a. Core task screens (needs M12)

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M14-01 | **Scaffold.** Project setup, API types generated from `openapi.yaml`, a typed client wrapper, lint, component test runner, Playwright, and a CI job that also checks generated types for drift. | `web/` | The app builds, an empty test suite runs in CI, and a changed contract without regenerated types fails CI | Routine, M | M12-05 |
| M14-02 | **Backend support for the browser.** CORS from `CORS_ALLOWED_ORIGINS`. `ETag` and `If-None-Match` on list endpoints, with the tag derived from a count and the latest `updated_at`. `updated_since` on `GET /tasks`, whose response also lists IDs archived since that time. Rate limits that tolerate polling. | `contract/openapi.yaml`, `internal/api/`, tests | Tests: an unchanged list returns `304`; a disallowed origin is refused; an archived task appears in the delta | Careful, M | M12-04 |
| M14-03 | **Sign-in and session.** Token held in memory with an `HttpOnly` refresh cookie, never in local storage. Tenant and role read from the token; admin pages hidden from members. The real sign-in redirect waits on owner action O2; until then a development sign-in accepts a token from `make dev-token`. | `web/src/auth/` | Tests: an expired session returns the user to sign-in; a member cannot see admin navigation | **Owner** (O2) for the real provider; development path Careful, M | M14-01 |
| M14-04 | **Task list and board.** Filterable, paginated list with quick add. Board with a column per status; dragging changes status, with a non-drag alternative. Polling every 15 to 30 seconds with conditional requests. Loading, empty, and error states. | `web/src/pages/tasks/` | Component tests cover the states; a disallowed status move shows the server's message | Routine, L | M14-02, M14-03 |
| M14-05 | **Task detail and editing.** Served at `/tasks/{id}`, the page every notification's view link opens when `APP_BASE_URL` is set (AP-10). Fields, comments, watchers, complete, archive, restore. Optimistic edits following the conflict flow in section 9.6: on `409`, roll back, reload, and tell the user their change was not saved. | `web/src/pages/task-detail/` | End-to-end test: two sessions edit one task; the second sees a conflict and loses nothing silently | Routine, L | M14-04 |
| M14-06 | **Notification feed backend.** Migration `000010` adding `notification_acks`. `GET /notifications/in-app` proxies AgentPulse with service credentials, taking tenant and user only from the token, and enriches each item with task data and read state; a timeout or open breaker returns `503` with a retry hint. `POST /notifications/{id}/ack` saves the ack locally first, then forwards feedback, retrying through the outbox on failure (G20). | `migrations/000010_notification_acks.{up,down}.sql`, `internal/api/notification_handler.go`, `internal/service/`, tests | Tests: a user cannot read another user's feed by changing the request; one ack yields one feedback call even when the first attempt fails | Careful, M | M12-04, M0-07 |
| M14-07 | **Notifications panel.** The in-app feed with act, snooze, and dismiss. When the feed is unavailable the panel shows an error and the rest of the app keeps working. If M0-07 finds AgentPulse has no feed endpoint, this task and M14-06 are dropped. | `web/src/pages/notifications/` | End-to-end test with AgentPulse unreachable: the task screens still work | Routine, M | M14-06 |

### M14b. Agent screens (needs M6, M8, M10)

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M14-08 | **Subtasks and dependency graph.** Children of a task, a graph of their dependency links, and a Decompose button that shows job progress. | `web/src/pages/task-detail/`, `web/src/components/` | Component tests cover a task with no children, a chain, and a diamond | Routine, M | M14-05, M8 |
| M14-09 | **Agent activity and approvals inbox.** A per-task timeline naming the agent, what changed, and its reasoning. An inbox of proposals awaiting the user, showing current and proposed values side by side, with approve and reject. | `web/src/pages/approvals/`, `web/src/components/` | End-to-end tests: approve a proposal; reject a proposal; a stale proposal shows a clear message | Routine, M | M14-05, M6 |
| M14-10 | **Morning brief.** A page at `/brief`, where the brief notification's view link points (AP-10), showing the caller's latest brief from `GET /me/brief`. | `web/src/pages/brief/` | Component tests cover a brief and the no-brief state | Routine, S | M14-05, M10-03 |

### M14c. Admin and delivery (needs M12)

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M14-11 | **Admin pages.** Learning suggestions with evidence and approve or reject; rule bootstrap; dead-letter rows with last error and replay; agent job list with run history and token use; workspace agent settings. | `web/src/pages/admin/` | End-to-end test: approve a learning suggestion. A member reaching an admin URL directly is refused by the server. | Routine, M | M14-05 |
| M14-12 | **Embedded build.** `web/embed.go` behind a build tag, with a stub otherwise, so `go build` works without a frontend build. Served at `/` when `SERVE_EMBEDDED_SPA=true`, falling back to `index.html` for client-side routes but never for paths under `/api/`. | `web/embed.go`, `web/embed_stub.go`, `cmd/taskify/main.go`, tests | Tests: plain `go build` succeeds; the tagged binary serves the app and does not shadow `/api/` | Routine, S | M14-04 |
| M14-13 | **Journeys, accessibility, and small screens.** The end-to-end journeys listed under M14 in [milestones.md](../milestones.md); keyboard navigation and labelled controls on the core flows; list, detail, and approvals usable at phone width; a cross-tenant task URL does not load. | `web/e2e/` | All journeys pass in CI | Routine, M | M14-09, M14-11 |

---

## M15. Python client SDK

Goal: Python scripts, pipelines, and agent workflows use Taskify through a typed client (feature C2). Design in section 9.4.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M15-01 | **Package and generated models.** `pyproject.toml` with distribution name `taskify-sdk` and import name `taskify`, Python 3.10 or later. Pydantic models generated from `openapi.yaml` into `_generated/`. A CI check that regeneration produces no difference. | `sdk/python/` | A changed contract without regenerated models fails CI | Routine, S | M12-05 |
| M15-02 | **Client core.** Sync and async clients on `httpx` sharing resource definitions; bearer token passed in, read from `TASKIFY_TOKEN`, or supplied by a callable; one typed exception per error code, with `VersionConflictError` carrying the current version; retries with backoff for `GET` only; SDK version in `User-Agent`; unknown response fields ignored. | `sdk/python/src/taskify/client.py`, `errors.py`, `auth.py` | Tests: each error code maps to its exception; a write is never retried; the token appears in no log line or exception message | Routine, M | M15-01 |
| M15-03 | **Resources.** Tasks, comments, agent jobs, notifications, admin. Cursor-following iterators; `update()` sends the object's `version` as `If-Match`; `decompose()` returns a job with `wait(timeout=...)`. | `sdk/python/src/taskify/resources/`, `pagination.py` | Tests against a local Go server cover create, paginated list, update, conflict, comment, complete, decompose and wait, approve | Routine, M | M15-02; job resources need M6 and M8 |
| M15-04 | **Quality gates.** `ruff`, `mypy`, and `pytest` in CI, with the Go server started for the integration tests. | CI, `sdk/python/tests/` | The SDK job is green | Routine, S | M15-03 |
| M15-05 | **README and distribution.** Usage examples and the Git-tag install command. No public wheels before v1.0 (decision 21). Reserving the name on the public index is owner action O3. | `sdk/python/README.md` | The install command works against a tag | Routine, S; reservation is **Owner** | M15-04 |

R3 is complete when every exit criterion for M12 to M15 in [milestones.md](../milestones.md) passes.
