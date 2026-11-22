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

- [r2-agents.md](r2-agents.md): M5 to M11, 33 tasks. M5 to M9 are done.
- [r3-clients.md](r3-clients.md): M12 to M15, 32 tasks. Not started.
- R1 (M0 to M4, 40 tasks) is complete; its task list is archived in [archive/r1-task-service.md](../archive/r1-task-service.md). Its one open task, M0-07, is owner action O1 below.

Finished and superseded documents are kept in [docs/archive](../archive/README.md).

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
| G13 | The blocker escalation notification has no event type | Decided by the owner, see D30: a new event type `task.escalated`, sent to the assignee of each open blocking task (creator if unassigned), once per recipient per job. Was: `task.blocked` with `payload.escalation: true`, key suffix `:escalation:{job_id}`. | Owner decided (D30) |
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
10. **Commit messages follow Conventional Commits** (`feat:`, `fix:`, `docs:`, ...): they decide the next release version. See [releasing.md](../releasing.md). Commit messages carry no `Co-Authored-By` trailer.
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
| M5 Agent runtime | 11 | Done. All exit criteria have tests, passing with `-race` on memory and PostgreSQL |
| M6 HITL bridge | 4 | Done. All exit criteria have tests, passing with `-race` on memory and PostgreSQL |
| M7 Triage | 4 | Done. All exit criteria have tests, passing with `-race` on memory and PostgreSQL |
| M8 Decomposer | 4 | Done. All exit criteria have tests, passing with `-race` on memory and PostgreSQL |
| M9 Blocker | 4 | Done. All exit criteria have tests, passing with `-race` on memory and PostgreSQL. R2 core complete |
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
| D15 | M5-01 | `tasks.parent_id` is `ON DELETE SET NULL`, not `CASCADE` as in [agentic.md](../archive/agentic.md) section 4.A. | A cascade would delete children inside a parent's permanent delete without their outbox scrubbing, agent job cleanup, or dependency resolution. |
| D16 | M5-03 | Dependency inserts take a per-tenant advisory lock (`pg_advisory_xact_lock`), then lock the blocker row before the dependent row, instead of locking the two tasks in ID order. | Locking only the two linked tasks lets inserts on disjoint pairs each pass the cycle check and together close a cycle through existing links (with Y→U and V→X present, X→Y and U→V lock different rows). Blocker-before-dependent is the order in which completing a blocker locks its dependents, so the two paths cannot deadlock. |
| D17 | M5-04 | Keys for `task.dependency_added`, `task.dependency_resolved`, and `task.dependency_reopened` are `...:{event_type}:{blocker_id}:v{blocker_version}`. | These notices leave the dependent's version unchanged, so the section 4.4 format would give two notices about different blockers the same key. |
| D18 | M5-04 | On permanent delete, dependents are re-checked after the task row is deleted, in the same transaction, not before. Their events name the blocker with status `deleted`. No `task.dependency_resolved` goes to a completed or archived dependent. | Before the delete the task still counts as an open blocker, so the check would find nothing to release. A finished dependent cannot act on the news. |
| D20 | M5-07 | The LLM client retries only 429 and 503 (three times by default, full-jitter backoff up to 8s, or a longer `Retry-After` capped at 30s). A per-call timeout is returned as `ErrTimeout` and not retried. `MockLLM` is `llm.Mock`, whose script is a list of steps (a response or an error) rather than responses only. | Retrying a 30-second timeout inside the call could hold a job for minutes; the runtime's attempt count (M5-09) is the place to try again. Scripted errors let runtime tests cover a failing model. |
| D21 | M5-08 | The registry checks task IDs by argument name. `task_id` and `parent_task_id` are targets and must be the job's task or a direct child of it; `depends_on_task_ids` are references and need only be in the job's tenant. A job with no task (digest, learning) may target any task in its tenant. A tool that reports a tier other than 1 is treated as Tier 2. `domain.Task` gains `ParentID`, mapping the M5-01 column: set on create, never changed by an update. | Section 5.3 limits calls to the job's task and its children, but `LinkDependencies` must name existing tasks to link to; a reference changes nothing about the referenced task. The children check needs the parent link, which M5-01 stored but nothing read. |
| D22 | M5-08 | Tool arguments are validated with a standard-library validator (`internal/agent/tools/schema.go`) instead of `santhosh-tekuri/jsonschema` from G19. It supports the JSON Schema 2020-12 subset that OpenAI-compatible providers accept for tool parameters: `type`, `properties`, `required`, boolean `additionalProperties`, `items`, `enum`, `minItems`, `maxItems`, `uniqueItems`, `minLength`, `maxLength`, `minimum`, `maximum`, and `format` (`uuid`, `date-time`, `date`). A schema using any other keyword is refused at registration. | Owner direction: imports from standard packages. Refusing unsupported keywords means no rule in a schema is silently unenforced, and keeps tool schemas portable across model providers. |
| D23 | M5-09 | A job's `updated_at` is its lease: `AgentJobRepository.Update` matches on status and `updated_at`, and the runtime renews it after every turn. `Claim` takes only the agent types the runtime has, so jobs for agents not yet built stay pending. A run that stops at `AGENT_MAX_TURNS` completes with `stopped: max_turns`. A job with no task names its workspace in `input_context.workspace_id`. Several tool calls in one turn share that turn's history row as lists, with `tool_name` `multiple`. A Tier 1 tool's output may report the task's new version (`tools.VersionedOutput`) so later calls in the run expect it. The `Agent` interface has no `Triggers()`. | A status check alone would let a worker whose job was recovered and claimed again overwrite it. The heartbeat keeps a slow but healthy run (up to six 30-second calls with retries) from being recovered at 5 minutes. Without the type filter, enabling agents before M7 would fail every queued job. Without version reporting an agent's second write would conflict with its first. Triggers are decided in the domain (M5-06). |
| G24 | M5-09 | Resolved by D24 (owner decisions). Was: open points left as the task row states them: a version conflict returns the job to `pending` without using an attempt, so a task edited continuously keeps its job re-running; a failed attempt is retried at the next poll with no backoff, as `agent_jobs` has no `next_attempt_at`; Tier 1 calls that ran before a second malformed answer stay applied, each being its own transaction. Proposals are saved with `awaiting_approval` now; the approval notification is M6-01. | Revisit if real traffic shows conflict loops or retry storms. |
| D24 | M5-09 | Migration `000005` adds `agent_jobs.retry_after` and `conflict_count`. A failed attempt waits 30s, 2m, then 10m before it can be claimed again. A version conflict waits 10s; the first 5 conflicts are free and each later one uses an attempt. Tier 1 calls that ran before a failure stay applied, and `execution_result` records every run's outcome and the calls that ran, failed runs included. | Owner decisions on G24: no retry storm during a model outage, no endless re-runs on a task under constant editing, and no transaction held open across model calls. |
| D25 | M6-02 | Repeating the decision a job already has is a no-op (`applied`/`dismissed` false); the opposite decision on a decided job returns `409 already_decided`, and so does any decision on a failed job, except that approving a job that failed with `stale_version` returns `stale_version` again. A decision before the job asks is `409 not_awaiting_approval`. Approved proposals run as the agent, not the person who approved them, and are checked again against their schema and the job's scope. If any of them fails, all are rolled back and the job fails. A job without a task can be decided by an admin only. | The plan treats every decision on a decided job as a no-op; the owner asked that contradicting a decision be refused. Recording applied changes as the person's would queue triage on its own proposals. |
| D26 | M7 | A tool's business-rule refusal (validation, invalid transition, not found) goes back to the model as a rejected tool result and the run carries on; it does not use the repair turn. A proposed priority is stored as `priority_recommendation` through a `Proposer` hook that does not move the task's version, so the proposal stays current. Similar tasks use PostgreSQL English full-text search on titles with no index; the context holds the 5 most recent comments and 30 most used labels. `agent_metadata.runs` keeps the last 20 entries; estimates are 1 to 28,800 minutes. The app registers the tools even with agents off, so earlier proposals can still be approved. | M7-02 asks for a cycle to be returned to the model as a tool error. A version bump from storing the recommendation would make the proposal stale at once. An index is not needed at triage's volume of one query per job. |
| G25 | M7-04 | Resolved by D27 (owner decision). Was: a priority recommendation kept acting as the priority hint after a person rejected the proposal. | |
| D27 | M7-04 | Dismissing a job clears the task's active `priority_recommendation` when it came from that job, so later notifications fall back to the task's own priority. The rejected recommendation is kept in `agent_metadata.rejected_priority_recommendations` with `rejected_by` and `rejected_at` (last 20). The task's version does not move. A recommendation from another job is left alone. | Owner decision on G25: routing on a priority a person rejected defeats the approval step and adds noise. |
| D28 | M8-01 | `CreateSubtaskDAG` checks its plan before the tier is decided, through a new optional `tools.Checker` hook: a parent other than the job's task, more subtasks than the cap, a repeated title, a `depends_on` title that names no sibling, and a cycle are all refused as invalid arguments, so they use the run's one repair turn and are never stored as proposals. Subtasks take the parent's workspace, priority, and assignee; their creator is the person who asked for the decomposition (`input_context.actor_id`, now `Scope.RequestedBy`). A blocked subtask sends no event at all until `task.unblocked`. The parent records the run in `agent_metadata.runs` (with `subtasks` and `subtask_ids`) without moving its version. A job creates subtasks at most once: a retry, or applying the same approval again, returns the subtasks already made. The links skip the tenant lock of D16. At execution the service checks the plan again against the workspace's current cap, and refuses a completed or archived parent. | The repair turn gives M8-03's "asked once to reduce, fails on a second over-cap plan" without a separate counter, and a refused plan must not reach a person for approval. The requester is the person accountable for the split; the agent cannot be a creator who decides on proposals or hears of completions. Moving the parent's version would make a pending proposal on it stale for no edit (as D26). Without the once-per-job rule, a run that failed after its decomposition committed would split the task again on retry. Links among tasks created in the same transaction cannot meet a concurrent insert, since no other transaction can see those tasks. |
| D29 | M8-02 | `POST /tasks/{id}/decompose` returns `202` with the agent job (as `GET /agent-jobs/{id}` shows it) and a `Location` header. While a decomposer job for the task is pending, running, or awaiting approval, that job is returned instead of a second one; the task row is locked so concurrent requests agree. Each new job has the dedup key `decomposer:{task_id}:request:{uuid}`. `409 agent_disabled` (G21) covers agents switched off for the workspace as well as a disabled decomposer; a completed or archived task is `422`. A subtask may itself be decomposed; nothing limits nesting. The comment trigger matches a body that is exactly `/decompose`, trimmed, in any case, written by a person. The comment is always kept: with the decomposer off it queues nothing and is counted as skipped, and a repeat while a job is open counts as `duplicate`. | Decomposition has no task version to deduplicate on, since creating subtasks leaves the parent's version alone, so a version-based key would block every later request. Section 5.7 names the error only for the explicit request; failing a comment because of a workspace setting would lose what the person wrote. |
| D30 | M9-03 | Owner decision on G13. The blocker escalation is a new event type, `task.escalated`. It goes to the owner (assignee, or creator when unassigned) of each open task the investigated task depends on: one event per distinct owner, naming that owner's blockers in `payload.blockingTasks`, never to the investigated task's assignee or watchers. Key `taskify:{tenant_id}:{task_id}:task.escalated:{job_id}:{recipient}`. It carries `jobId`, `agentType`, `bypassDigest`, a `priorityHint` of at least `high`, and `blockedReason` when the task is blocked; while the task is blocked it offers the action `unblock` (`actionUrls.unblock` is the task's URL, `actionLabels.unblock` is "Unblock and set In Progress"), which reaches Taskify as webhook action `unblock` (G7). It is built by Go code, not a model tool, through an optional `agent.Finisher` hook that the runtime calls inside the transaction that saves the job as `completed` or `awaiting_approval`, so it is written with the run's result, once; a run that fails or will be retried sends none. The open blockers are read when the run ends, so one the run linked is included. The default AgentPulse rules are unchanged: `bypassDigest` already delivers it at once, and a rule can now match on the event type. | The owner asked for a separate event type so AgentPulse rules can tell an escalation from a block. Writing it with the job's result keeps the model out of who is notified and stops a retried run from escalating twice; the key would stop a repeat anyway. Changing the existing rule would not reach workspaces already bootstrapped. |
| D31 | M9-03 | Owner decision. When the investigated task has no open blocking task (a manual block whose dependencies are all completed or archived or that has none, or a stale task with no open dependency), the run posts the resolution brief only and sends no escalation. A stale `in_progress` task that does wait on an open dependency is escalated, without the `unblock` action, since it is not blocked. | There is nobody to escalate to; the brief still tells the task's people what the agent found. |
| D32 | M9-01 | `DraftResolutionComment`: a job comments on a task at most once (`agent_metadata` run with `comment_id`); a retry returns the earlier comment. | A retried run must not post the brief twice. |
| D33 | M9-02 | A task is stale when it is `in_progress` and neither its `updated_at` nor any comment (from anyone, the agent's own brief included) is newer than 48 hours (`domain.StaleAfter`). `TaskRepository.ClaimStale` selects these with `FOR UPDATE SKIP LOCKED` across tenants, only in workspaces whose settings allow the blocker investigator, and skips tasks that already have today's job (`blocker_investigator:{task_id}:stale:{YYYY-MM-DD}`, UTC); the unique dedup key stops a second job if two scans overlap. The job's input context is `{"trigger": "stale"}`, and its base version is the task's. The scan runs in every scheduler tick after the reminders, whatever `AGENTS_ENABLED` is, as task changes queue jobs whatever it is. | Without the edit check, a task moved to `in_progress` an hour ago would count as stale because its last comment is old. Counting the brief means an untouched task is investigated every two days rather than every day; the daily key is the upper bound in G12. Checking settings in the claim keeps disabled workspaces from being claimed on every tick. |
| D34 | M9-03 | The blocker investigator is offered `DraftResolutionComment`, `LinkDependencies`, `ReassignTask`, and `ShiftDueDate`, not `ArchiveTask`. Its context is the task (with assignee, creator, blocked reason, and due date), its 10 most recent comments, every task it depends on with that task's owner and whether it is still open, and up to 5 similar open tasks (G18) that are not already blockers. It runs for a manual `task.blocked` by a person and for the stale scan only; a dependency block, or any agent or system change, queues nothing. With `require_hitl_for_all`, the brief becomes a proposal, and the escalation goes out with the approval request. | `LinkDependencies` needs task IDs to link to, and the comments alone rarely contain them. The escalation does not depend on the model's output, so it is not held back by an approval. |
| D35 | M9-04 | `TaskService.UnblockTask` moves a `blocked` task to `in_progress`, whatever the block's reason, as the person named in the callback. A task already `in_progress` is left alone, and the webhook still forwards `acted`, as `complete` does for a completed task; any other status is `422`. | The person who owns the blocker has decided the task can go ahead. A second click on the same escalation must be harmless. |
| G23 | M5-03 | A person adding a dependency is mentioned in section 4.3, but section 5.4 has no endpoint for it. Only the service operation exists until an endpoint is specified. | Rule 12: not built ad hoc. |
| D19 | M5-06 | Skipped agent jobs are counted in `taskify_agent_jobs_skipped_total` (labels `agent_type`, `reason`: `agents_disabled`, `agent_disabled`, `duplicate`), a metric not listed in section 6.3. Only triage (task created, or edited by a person) and the blocker investigator (manual block) queue jobs from task changes. | The M5-06 row asks for a counter and section 6.3 names none. The other section 5.6 triggers have their own homes: the digest gatekeeper is a deterministic check (G14), dependency release is the synchronous re-check from M5-04, and `/decompose` is M8-02. |
