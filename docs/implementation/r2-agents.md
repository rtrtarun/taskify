# R2 Tasks: Agentic Task Plane (M5 to M11)

Read [README.md](README.md) first: it explains the levels (Routine, Careful, Owner), the defaults G1 to G22, and the rules every task follows. Section numbers below refer to [architecture.md](../architecture.md). Exit criteria for each milestone are in [milestones.md](../milestones.md).

R2 starts after R1 is released. All R1 tests must keep passing throughout.

Three rules apply to every task in this file:

- The model never touches the database. It can only request a registered tool, and typed Go code decides whether and how the request runs.
- Tenant and task scope come from the `agent_jobs` row, never from model output.
- A change authored by an agent or by the system never enqueues an agent job.

Tests use the mock LLM. No test in CI needs a real model.

---

## M5. Agent runtime and schema (done)

Goal: a generic engine that can run any agent, plus the dependency model that agents and people both use.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M5-01 | **Migration 000004.** On `tasks`: `parent_id` (self-reference), `estimated_minutes`, `agent_metadata`. `task_dependencies` with the composite primary key, both foreign keys cascading, `tenant_id`, a check against self-dependency, and the reverse index. `workspaces.agent_settings` with the defaults in section 5.7. `agent_jobs` per section 4.1 and the changes in section 7: statuses including `awaiting_approval` and `dismissed`, nullable `task_id`, unique `dedup_key`, `base_task_version`, `attempts`, `decided_by`, `decided_at`. `agent_run_history`. The indexes in section 4.2. | `migrations/000004_agent_schema.{up,down}.sql` | Up, down, and up again run cleanly on a database holding R1 data | Routine, M | R1 |
| M5-02 | **Agent job and settings types.** `AgentJob`, its statuses and allowed transitions (section 4.3), `SafetyTier`, dedup key builders (section 4.4), and `AgentSettings` with parsing and defaults. | `internal/domain/agentjob.go` and test | Tests cover every job transition and the default settings of a new workspace | Routine, S | M5-01 |
| M5-03 | **Adding dependencies.** A service operation that inserts a `task_dependencies` row after checking both tasks share a tenant and that the link closes no cycle. The cycle check is a recursive query run while the involved tasks are locked in ID order (section 4.3). Effect on the dependent: a `todo` task receives a dependency block; an `in_progress` task keeps its status and gets a system comment and `task.dependency_added`. | `internal/service/dependency_service.go`, `internal/repository/postgres/dependency_repo.go`, memory equivalent, tests | Tests: a cycle is rejected, including when two requests race; a cross-tenant link is rejected; an `in_progress` dependent does not change status | Careful, L | M5-01 |
| M5-04 | **Dependency resolution.** Inside the transaction that completes or archives a task, re-check its dependents following the sequence in section 4.3: lift dependency blocks and emit `task.unblocked`; leave manual blocks in place with a system comment and `task.dependency_resolved`. On restore or reopen of a blocker: re-block `todo` dependents, and for started or completed dependents add a comment and a high-priority `task.dependency_reopened`. System-applied blocks use actor kind `system`, carry `blockedReason: "dependency"`, and omit `bypassDigest`. Manual blocks get `bypassDigest` and a `priorityHint` of at least `high`. Extend permanent delete (M3-05) to delete agent jobs first and run this check before the task row goes. | `internal/service/task_service.go`, `dependency_service.go`, `internal/domain/event.go`, tests | The eight dependency exit criteria under M5 in [milestones.md](../milestones.md) each have a passing test | Careful, L | M5-03 |
| M5-05 | **Workspace agent settings.** Extend `GET` and `PATCH /workspaces/{id}/settings` with `agent_settings`. Reject unknown agent names and a non-positive subtask cap. | `internal/api/admin_handler.go`, `internal/service/workspace_service.go`, tests | Tests: a new workspace reports agents off and approval for everything on; an invalid patch is refused | Routine, S | M5-02 |
| M5-06 | **Job queue and enqueueing.** Agent job repository: insert with `ON CONFLICT (dedup_key) DO NOTHING`, claim with `SKIP LOCKED`, status updates, run history insert. Replace the no-op hook from M2-05: within the task transaction, a user-authored event that matches an agent trigger (section 5.6) writes a job row, provided `agents_enabled` is true and the agent is not in `disabled_agents`. A skipped job increments a counter. | `internal/repository/postgres/agent_job_repo.go`, memory equivalent, `internal/service/task_service.go`, tests | Tests: agent-authored and system-authored changes write no job; with agents off no job row exists; the same event twice writes one job | Careful, M | M5-02 |
| M5-07 | **LLM client and mock.** `LLMClient` interface (section 5.3). An OpenAI-compatible chat completion client with tool calling, a 30-second timeout, and exponential backoff on 429 and 503. `MockLLM` replays a scripted list of responses and records what it was sent. | `internal/agent/llm/client.go`, `mock.go`, tests | Tests against a stub HTTP server cover a tool-call response, a timeout, and retry on 429 and 503 | Routine, M | R1 |
| M5-08 | **Tool registry.** `Tool` interface, `Scope`, and `ToolRegistry` (section 5.3). `Dispatch` rejects an unregistered tool, validates arguments against the tool's JSON Schema, rejects any `task_id` outside the scope's tenant or outside the job's task and its children, and asks the tool for its tier. With `require_hitl_for_all`, every call is Tier 2. Tier 1 calls execute through `TaskService` with the expected version and actor kind `agent`; Tier 2 calls are returned as proposals without executing. | `internal/agent/tools/registry.go` and test | Tests: unknown tool, bad arguments, and another tenant's task are each rejected; `require_hitl_for_all` turns a Tier 1 call into a proposal | Careful, M | M5-02 |
| M5-09 | **Agent runtime.** The loop in section 3.6. Claim pending jobs; re-check workspace settings at claim; run at most `AGENT_MAX_TURNS` turns; write one `agent_run_history` row per turn; give malformed output one repair turn and then fail the job with no data changed; on `ErrVersionConflict` return the job to `pending`; on other errors count an attempt and fail at the limit. Recover jobs stuck in `running` (G11). Nothing runs unless `AGENTS_ENABLED` is true. | `internal/agent/runtime.go`, `agent.go`, tests | Tests with `-race`: two runtimes never run the same job; a version conflict re-runs with fresh context; a job queued before agents were switched off does not run | Careful, L | M5-06, M5-07, M5-08 |
| M5-10 | **Reference agent and metrics.** The `Agent` interface (section 5.3) and a minimal test agent that proves the engine end to end. The three R2 metrics in section 6.3. Agent jobs carry the originating trace in `input_context`. | `internal/agent/agent.go`, `internal/observability/metrics.go`, tests | Test: a scripted Tier 1 tool call changes the task and is recorded in history | Routine, M | M5-09 |
| M5-11 | **Local model setup.** A compose profile running Ollama with `qwen2.5-coder:7b` or `llama3.1:8b`, the `LLM_*` variables documented, and one opt-in smoke test that is skipped unless `LLM_BASE_URL` is set. | `docker-compose.yml`, `README.md`, `tests/e2e/` | The smoke test passes locally against Ollama and is skipped in CI | Routine, S | M5-10 |

---

## M6. Human-in-the-loop bridge (done)

Goal: high-impact proposals wait for a person, who can decide from a notification, the CLI, or the frontend.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M6-01 | **Storing proposals.** When a run produces Tier 2 calls, the runtime saves them in `proposed_actions`, records `base_task_version`, sets `awaiting_approval`, and writes an `agent.approval_requested` outbox row in the same transaction. The notification goes to the assignee, or the creator when unassigned, and carries approve and reject `actionUrls`. | `internal/agent/runtime.go`, `internal/domain/event.go`, tests | Test: a Tier 2 proposal changes no task data and produces one approval notification | Routine, M | M5-10 |
| M6-02 | **Apply and dismiss.** `AgentJobService.Apply` and `Dismiss`, section 3.7. Lock the job row; a job not in `awaiting_approval` is a no-op; a task whose version differs from `base_task_version` fails the job with `stale_version`; otherwise apply the proposed actions through `TaskService`, with their outbox events, and set `completed`, `decided_by`, `decided_at`, all in one transaction. Dismiss sets `dismissed` and changes no task data. Who may decide follows G10. | `internal/service/agent_job_service.go` and test | Tests: approve applies once; a second apply or dismiss is a no-op; a stale proposal fails safely; an unauthorised user gets a permission error | Careful, M | M6-01 |
| M6-03 | **Job endpoints.** `GET /agent-jobs/{id}`, `POST /agent-jobs/{id}/apply`, `POST /agent-jobs/{id}/dismiss`, all requiring a user JWT. | `internal/api/agent_job_handler.go` and test | Handler tests cover success, not found, `409` on stale version, and `403` | Routine, S | M6-02 |
| M6-04 | **Webhook routing.** Extend the webhook handler with `approve` and `reject` actions that call the same service. After each decision, send feedback to AgentPulse: `acted` on approve, `dismissed` on reject. | `internal/api/webhook_handler.go` and test | Test: a signed approve callback applies the proposal and the mock receives one `acted` signal | Routine, S | M6-02 |

---

## M7. Triage agent (done)

Goal: new tasks are enriched automatically (feature A1).

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M7-01 | **`UpdateTaskAttributes` tool.** Arguments per section 5.7. Tier 1 for labels, description, and estimate; Tier 2 when `priority` is set. Each Tier 1 execution appends an entry to `agent_metadata.runs` in the shape shown in section 5.7. | `internal/agent/tools/update_task_attributes.go` and test | Tests cover both tiers and the metadata entry | Routine, M | M6-02 |
| M7-02 | **`LinkDependencies` tool.** Tier 1. Calls the dependency service from M5-03, so cycle, tenant, and blocking rules are not reimplemented. | `internal/agent/tools/link_dependencies.go` and test | Test: a link that would close a cycle is returned to the model as a tool error | Routine, S | M5-03, M5-08 |
| M7-03 | **Triage agent.** Triggers: `task.created` and user-authored `task.updated`, including tasks where creator and assignee are the same. Allowed tools: the two above. Context: the task, recent comments, the workspace's existing labels, and similar open tasks (G18). | `internal/agent/triage.go` and test | Test with the mock model: a bare task gains labels, a description, and an estimate, and `agent_metadata` records what changed and why | Routine, M | M7-01, M7-02 |
| M7-04 | **Priority recommendation.** A recommended priority is stored in `agent_metadata.priority_recommendation` and raised as a Tier 2 proposal. Later notifications for the task use the recommendation as `priorityHint`. | `internal/agent/triage.go`, `internal/domain/event.go`, tests | Tests: the recommendation produces an approval request, not a change; a human edit made during triage is not overwritten and the job re-runs | Routine, S | M7-03 |

---

## M8. Decomposer agent (done)

Goal: a high-level task becomes a set of linked subtasks (feature A3).

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M8-01 | **`CreateSubtaskDAG` tool.** Section 3.8. Resolve `depends_on` titles to siblings and reject unknown titles; sort topologically and reject cycles; enforce `max_subtasks_per_decomposition`. In one transaction: insert the children with `parent_id`, those with dependencies as `blocked` with reason `dependency` and the rest as `todo`; insert the dependency rows; write `task.created` only for children with no blockers. | `internal/agent/tools/create_subtask_dag.go` and test | Tests: an injected failure mid-batch leaves no child behind; cyclic and unknown `depends_on` are rejected; no immediate `task.blocked` notification and no blocker job results | Careful, M | M5-04, M5-08 |
| M8-02 | **Triggers.** `POST /tasks/{id}/decompose` writes a decomposer job and returns `202` with the job ID. A comment whose body is the `/decompose` command does the same. A disabled decomposer returns an error instead (G21). | `internal/api/task_handler.go`, `internal/service/task_service.go`, tests | Tests cover both triggers and the disabled case | Routine, S | M5-06 |
| M8-03 | **Decomposer agent.** Context: the parent task and an instruction to produce subtasks with `depends_on`. If the model proposes more subtasks than the cap, it is asked once to reduce them. | `internal/agent/decomposer.go` and test | Test with the mock model: an over-cap proposal followed by a valid one succeeds; two over-cap proposals fail the job | Routine, S | M8-01, M8-02 |
| M8-04 | **Readiness notification.** Confirm that completing the last blocker of a child uses the resolution logic from M5-04 and emits exactly one `task.unblocked`. No new mechanism. | tests in `internal/service/` | Test: a child with two blockers is notified once, after the second completes | Routine, S | M8-01 |

---

## M9. Blocker agent (done)

Goal: blocked and stale tasks are investigated and escalated (feature A2). This completes the R2 core.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M9-01 | **Blocker tools.** `DraftResolutionComment` (Tier 1; the comment is authored by the agent). `ReassignTask`, `ShiftDueDate`, and `ArchiveTask` (Tier 2; any due-date shift needs approval, section 5.7). | `internal/agent/tools/` one file per tool, tests | Tests cover each tool's tier and its effect when applied | Routine, M | M6-02 |
| M9-02 | **Stale scan.** The deadline scheduler also enqueues `blocker_investigator` jobs for tasks `in_progress` with no comment for 48 hours, using the stale dedup key in section 4.4 (G12). | `internal/worker/deadline_scheduler.go` and test | Test: repeated scans in one day produce one job per stale task | Routine, S | M5-06 |
| M9-03 | **Blocker agent.** Triggered by manual `task.blocked` only, and by the stale scan. Reads comments and dependencies, writes a resolution brief as a comment, and sends an escalation with an "Unblock and set In Progress" action (G13). | `internal/agent/blocker.go`, `internal/domain/event.go`, tests | Tests: the brief appears as an agent comment; the escalation goes to the owner of the blocking task, not to the blocked task's assignee; a dependency block starts no job | Routine, M | M9-01, M9-02 |
| M9-04 | **Unblock action.** The webhook accepts `unblock`, moving the task from `blocked` to `in_progress`. | `internal/api/webhook_handler.go` and test | Test: a signed unblock callback changes the status and records `acted` | Routine, S | M9-03 |

---

## M10. Digest agent (done)

Goal: protect overloaded users and produce a morning brief (feature A4).

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M10-01 | **Load gatekeeper.** When a task is assigned, count the assignee's `in_progress` `p1_critical` tasks. If the count is at or above the threshold and the new task is not critical, write the notification with `channels: ["digest"]` and `payload.digestSummary` set to "Deferred to avoid context switching". Critical and blocked events keep `bypassDigest`. This is a query in the task transaction, not a model call (G14). | `internal/service/task_service.go`, `internal/domain/event.go`, tests | Tests: a non-critical assignment to an overloaded user is digest-only; a critical one is immediate; with the digest agent disabled nothing is deferred | Careful, M | M7-04 |
| M10-02 | **Morning brief.** The scheduler enqueues one user-scoped digest job per user per day (G15), with `task_id` null. The agent gathers the top three items, pending approvals, and newly unblocked tasks, has the model write the text, and sends it on the digest channel. A user with nothing to report gets no brief. | `internal/agent/digest.go`, `internal/worker/deadline_scheduler.go`, tests | Tests: one brief per user per day; an empty brief is skipped | Routine, M | M10-01 |
| M10-03 | **Brief endpoint.** `GET /api/v1/me/brief` returns the caller's most recent brief (G15), for the frontend in M14-10. | `internal/api/agent_job_handler.go` and test | Test: a user sees only their own brief | Routine, S | M10-02 |

---

## AP. AgentPulse v1 alignment (done)

Goal: Taskify's AgentPulse integration matches the API AgentPulse is building in its milestone 40 (slices 40a to 40d), as agreed in [blockers.md](../blockers.md). The owner decided every point on 2026-11-22. AP runs before M11, because M11 uses the `/v1/` learning and analytics endpoints. AgentPulse's 40b refuses an unknown action key with a 400, so AP-02 must ship before 40b is deployed. AP-10 and AP-11 are the owner's decisions after AP was reviewed, and AP-12 applies three details AgentPulse confirmed afterwards, all on 2026-11-22.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| AP-01 | **`/v1/` paths and mock.** The client calls `POST /v1/notify`, `POST /v1/feedback`, `POST /v1/rules`, `GET /v1/rules`, `/v1/learning/suggestions...`, and `GET /v1/tenants/{t}/users/{u}/in-app-feed` (R8 moves from `/internal/v1/...`). It keeps `Authorization: Bearer` and `/healthz`. The mock serves the same paths and enforces AgentPulse's 40b rules: a tenant ID matching `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` and not `default`; `actionUrls` keys limited to `complete`, `snooze_2h`, `approve`, `reject`, `unblock`, `view`; `taskId` required for `complete`, `snooze_2h`, `unblock`; `jobId` required for `approve`, `reject`. A notification with only `view`, or no `actionUrls`, is valid. | `internal/agentpulse/`, `tests/mockagentpulse/`, tests | Tests assert the exact paths and bodies; the mock refuses each invalid case with a 400 naming it | Routine, M | M10 |
| AP-02 | **Action keys.** Task notifications send `complete` (the "Mark done" button, pointing at `POST /api/v1/tasks/{id}/complete`) instead of `acknowledge`. Every notification Taskify builds uses only the six keys, with the IDs AP-01 requires. | `internal/domain/event.go`, tests | Test: every event type's payload passes the mock's action checks | Routine, S | AP-01 |
| AP-03 | **Critical rules.** Rules carry an optional `attentionClass` (`critical`, `standard`, `background`). The default `critical-or-blocked` rule sets `critical` and gains an entry for `eventTypes: ["task.escalated"]`, so p1_critical tasks, manual blocks, and escalations break through quiet hours. Every entry of a critical rule has a real condition. | `internal/agentpulse/client.go`, `internal/service/workspace_service.go`, tests | Test: the exact rule bodies sent at workspace creation | Routine, S | AP-01 |
| AP-04 | **Tenant IDs.** Every identity source (JWT claim, development headers, `cmd/devtoken`) and `domain` validation accept only AgentPulse's pattern, compared case-sensitively and never trimmed, and refuse `default`. | `internal/api/auth.go`, `internal/domain/`, `cmd/devtoken`, tests | Tests: valid and invalid tenant IDs at each entry point; `tenant_dev` still works | Careful, S | none |
| AP-05 | **Contacts.** `PUT /api/v1/me/contact` with `{email, slack_user_id, phone}` (each optional, `null` clears it) for the caller only. Taskify stores the latest contact per (tenant, user) and forwards it through the outbox (an `event_type = 'contact.update'` row, as G20 does for feedback) to `PUT /v1/tenants/{t}/users/{u}/contact` with `{email, slackUserId, phone}`, always sending all three fields. AgentPulse treats the PUT as a full replace and a repeat as a no-op. Two updates close together must not leave AgentPulse with the older one, whatever order the relay sends them in. `GET /api/v1/me/contact` returns the stored contact. | migration `000007`, `internal/api/`, `internal/service/`, `internal/agentpulse/`, `internal/worker/outbox_relay.go`, mock, tests | Tests: a user can only set their own contact; validation; one update yields one AgentPulse call even when the first attempt fails; an older update never overwrites a newer one | Careful, M | AP-01 |
| AP-06 | **Callback IDs.** The webhook body may carry `callbackId` (`cb-` plus 32 hex characters), identical on every retry of one click while the timestamp and signature change. A callback is a duplicate when `(tenantId, callbackId)` was seen before; without a `callbackId`, the signature check of D10 still applies. | `internal/api/webhook_handler.go`, `internal/service/`, migration `000007`, tests | Tests: a retry signed again with a new timestamp is a duplicate and forwards no second feedback; a byte-identical replay is still a duplicate | Careful, S | none |
| AP-07 | **Digest line names the task.** A held assignment's `digestSummary` reads `Assigned to you: {title}. Deferred to avoid context switching`. | `internal/domain/event.go`, tests | Test updated | Routine, S | none |
| AP-08 | **Scrub briefs on permanent delete.** Deleting a task permanently also deletes, in the same transaction, stored morning briefs (digest jobs) that mention it and their unsent `digest.morning_brief` outbox rows. `GET /me/brief` then returns an older brief or `404`. | `internal/service/task_service.go`, repositories, tests | Test: after the delete no stored brief or pending notification names the task | Careful, S | none |
| AP-09 | **Documents.** Architecture section 5.5 describes the `/v1/` contract, contacts, the action-key table, `attentionClass`, and `callbackId`. Record departures. | `docs/architecture.md`, `docs/implementation/README.md` | Documents match the code | Routine, S | AP-01 to AP-08 |
| AP-10 | **View link.** Owner decision after AP's review. An `APP_BASE_URL` setting, the web app's address. Every task notification carries `actionUrls.view` `{APP_BASE_URL}/tasks/{id}`, and approvals and escalations view their task there too; the frontend serves that route in M14. While `APP_BASE_URL` is unset, task notifications carry no `view`, as before. | `internal/config/`, `internal/domain/`, callers, tests | Tests: every notification type carries the view link with the setting and none without it, and still passes the mock's 40b checks | Routine, S | AP-02 |
| AP-11 | **User IDs.** Owner decision after AP's review. Taskify accepts only user IDs of 1 to 128 printable ASCII characters, with no control characters, at every identity entry point (JWT subject, development headers, `cmd/devtoken`) and wherever domain validation checks one (assignee, watchers, creator, contact owner, webhook `userId`), as AP-04 does for tenant IDs. User IDs appear in idempotency keys, which AgentPulse accepts in printable ASCII only. | `internal/domain/`, `internal/api/auth.go`, `cmd/devtoken`, `internal/service/`, tests | Tests: valid and invalid user IDs at each entry point and in each validated field | Careful, S | AP-04 |
| AP-12 | **Confirmed details.** AgentPulse confirmed three details on 2026-11-22. Feedback: `notificationId` is optional (empty means absent), at least one of `notificationId` and `taskId` is required, and Taskify leaves `notificationId` out when it has none. Tenant scoping: every `/v1/` call that names an object by ID (rules, learning suggestions, notifications) carries `tenantId` (`400` without it), and another tenant's object answers `404`, like an unknown ID; the learning decision call sends `tenantId`. Contact formats: email a plain address of at most 254 characters with no display name, Slack `^[UW][A-Z0-9]+$` of at most 32 characters, phone `^\+[1-9][0-9]{1,14}$`, one bad field refusing the whole PUT with a `400` naming it; Taskify validates exactly the same. | `internal/agentpulse/`, `internal/api/admin_handler.go`, `internal/domain/contact.go`, mock, tests | Tests: feedback without a notification omits the field; the mock enforces each rule; Taskify and the mock accept and refuse the same contact values; a refused contact PUT is dead-lettered with its reason and not retried | Routine, S | AP-05 |

---

## M11. Learning agent

Goal: dismissal patterns become reviewable rule proposals (feature A5). This completes R2.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M11-01 | **Client additions.** `SubmitLearningSuggestion` with the body in section 5.5, and a read of AgentPulse feedback analytics (endpoint from M0-07, G16). | `internal/agentpulse/client.go`, `tests/mockagentpulse/`, tests | Tests assert the exact JSON sent | Routine, S | M0-07 |
| M11-02 | **Pattern aggregation.** Deterministic Go code that computes dismiss and mute ratios by event type and label, and discards patterns below AgentPulse's thresholds: 5 signals, 2 users, 30-day window. | `internal/agent/learning.go` and test | Tests: a consistently dismissed label is found; a pattern below any threshold is dropped | Routine, M | M11-01 |
| M11-03 | **Learning agent.** A tenant-scoped scheduled job. The model turns each surviving pattern into a proposal through the `ProposeNotificationRule` tool, with `proposedAdjustment` within ±20. Proposals are posted to AgentPulse, where they stay `PENDING_APPROVAL`, and appear through the existing admin endpoints from M3-09. | `internal/agent/learning.go`, `internal/agent/tools/propose_notification_rule.go`, tests | Tests: a synthetic history yields a proposal naming the label; an out-of-range adjustment is rejected; nothing takes effect without an admin decision | Routine, M | M11-02, M5-10 |

R2 is complete when every exit criterion for M5 to M11 in [milestones.md](../milestones.md) passes and all R1 tests still pass.
