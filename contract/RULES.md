# Taskify contract rules

The rules a schema cannot express. [openapi.yaml](openapi.yaml) is normative for paths, bodies, and status codes, [events/](events/) for outbox payloads, and [tools/](tools/) for what a model may request; this file is normative for everything else a client, a second server, or a reviewer needs (docs/architecture.md section 9.2). The Go server is the reference implementation. Each rule names the fixtures in [fixtures/](fixtures/) that encode it, and the conformance runner (`tests/conformance`) executes them against any server; `TestRulesCiteFixtures` checks that every numbered rule cites one and that every cited fixture exists. The few cases no API call can reach on demand (scheduled briefs and learning, the blocker agent, and dependency links, which have no endpoint yet, G23) say which Go tests cover them.

## 1. Formatting

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R1.1 Timestamps | RFC 3339 in UTC with a `Z` suffix, such as `2026-11-22T09:00:00Z`, with up to six fractional digits. A request may send any offset; the server stores and returns UTC. | `tasks/create.json`, `tasks/update-clears-fields.json` |
| R1.2 IDs | Task, comment, and agent job IDs are UUIDs in lower-case hyphenated form (version 7 for new rows). Workspace IDs are names chosen by an admin. | `tasks/create.json` |
| R1.3 Field names | snake_case in Taskify's API. The webhook callback, the relayed learning suggestions, and outbox payloads are AgentPulse's, in camelCase. | `webhooks/complete.json`, `events/task-created.json` |
| R1.4 Unknown fields | Clients ignore response fields they do not know. The server refuses unknown request fields with `400 invalid_request`, except in the webhook callback, which AgentPulse may extend. | `tasks/validation.json` |
| R1.5 Bodies | One JSON object of at most 1 MiB; a larger body is `413`. | `tasks/validation.json` |

## 2. Identity and tenancy

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R2.1 Credentials | A bearer JWT signed with the server's key (HS256 or RS256), with `sub` (the user), `tenant_id`, `exp`, and optional `role`. A missing, expired, or wrongly signed token is `401 unauthorized` with `WWW-Authenticate`. | `auth/*.json` |
| R2.2 Development headers | `X-Tenant-ID`, `X-User-ID`, and `X-User-Role` stand in for a token only with `ENV=development` or from a trusted gateway address. Otherwise they are refused with `401`. A request that sends a token is judged on the token alone. | `auth/headers-refused.json` |
| R2.3 Roles | `role: admin` is an admin; anything else is a member. Admin-only: workspaces, bootstrap-rules, learning suggestions, the outbox, and permanent delete. A member gets `403 forbidden`. | `auth/member-forbidden.json`, `delivery/dead-letter.json` |
| R2.4 Tenant | The tenant comes from the identity only, never from a path, query, or body. Tenant IDs match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, other than `default`, case-sensitive and never trimmed. User IDs are 1 to 128 characters from space to tilde, after trimming. | `auth/bad-tenant.json` |
| R2.5 Isolation | Another tenant's task, job, workspace, or outbox row answers `404 not_found`, as an unknown ID does. A malformed ID also answers `404`. | `tasks/tenant-isolation.json` |

## 3. Versions and conflicts

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R3.1 Version | A task starts at version 1, and every change that alters it adds 1. A change that alters nothing keeps the version. Reminders, priority recommendations, and agent run records do not change it. | `transitions/*.json` |
| R3.2 ETag | Responses carrying one task send `ETag: "{version}"`. | `tasks/create.json` |
| R3.3 Expected version | `PUT` and `PATCH` need the expected version in `If-Match` (`"3"`, `W/"3"`, or `3`) or in the body's `version`; both, if sent, must agree (`400`). Without either: `428 version_required`. Complete, restore, and archive take an optional `If-Match`. | `tasks/update-needs-version.json` |
| R3.4 Conflict | A stale version is `409 version_conflict` with `error.details.current_version`. Clients reload; they never retry blindly. | `tasks/stale-version.json` |
| R3.5 Patch semantics | Only fields present change. `null` clears a nullable field and empties a list; it is refused for `title`, `description`, `status`, and `priority` (`422`). `PUT` behaves as `PATCH`. | `tasks/update-clears-fields.json` |

## 4. Task status

R4.1, the transition matrix. A cell marked yes is allowed; no is `422 invalid_transition`, with nothing changed. Moving to the current status is not a transition: the task is returned unchanged (`200`, same version). One fixture per cell, `transitions/{from}-to-{to}.json`, sets the task to the first status and sends `PATCH` with the second.

| From \ To | `todo` | `in_progress` | `blocked` | `completed` | `archived` |
| --- | --- | --- | --- | --- | --- |
| `todo` | unchanged | yes | yes (manual) | yes | yes |
| `in_progress` | yes | unchanged | yes (manual) | yes | yes |
| `blocked` | yes | yes | unchanged | no | yes |
| `completed` | no | yes (reopen) | no | unchanged | yes |
| `archived` | yes (restore) | no | no | no | unchanged |

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R4.2 Side effects | Entering `completed` sets `completed_at`; leaving it for open work clears it. Entering `archived` sets `archived_at` and keeps `completed_at`; restoring clears `archived_at`. `blocked_reason` is set exactly while `blocked`. | `transitions/*.json` |
| R4.3 Actions | `POST /complete` moves to `completed` (repeating it changes nothing). `DELETE` archives (repeating it changes nothing). `POST /restore` moves `archived` to `todo` and refuses any other status with `422`. | `tasks/complete-archive-restore.json` |
| R4.4 Lists | Lists leave out archived tasks unless `status=archived`. A fetch by ID returns an archived task. | `tasks/complete-archive-restore.json` |
| R4.5 Block reasons | A person's move to `blocked` is `manual`, lifted by a person only. The server applies `dependency` to a `todo` task with an open dependency and lifts it, back to the status before, when the last dependency is completed or archived. A task already started is never blocked by the server. | `transitions/todo-to-blocked.json` |
| R4.6 Permanent delete | `DELETE ?permanent=true`, admin only, answers `204` and removes the task, its comments, watchers, dependency links, agent jobs, outbox rows in any status, and the morning briefs naming it. `permanent` accepts only `true` and `false`. | `tasks/permanent-delete.json` |

## 5. Events and notifications

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R5.1 What raises what | Creating with an assignee: `task.created`. A new assignee: `task.assigned`. Title, description, or due date: `task.updated`. A higher priority: `task.priority_escalated`. Entering `blocked`: `task.blocked`. Entering `completed`: `task.completed`. A comment: `task.comment_added`. Other status changes raise nothing. The full list is in docs/architecture.md section 5.6. | `events/*.json` |
| R5.2 Recipients | `task.created`, `task.assigned`, reminders, and dependency notices go to the assignee; `task.updated`, `task.priority_escalated`, and `task.blocked` to the assignee and watchers; `task.completed` to the creator and watchers; `task.comment_added` to the assignee, watchers, and the people mentioned (`isMentioned`). The person who acted is never notified of their own action. One outbox row per recipient. | `events/*.json` |
| R5.3 Routing hints | `priorityHint` maps `p1_critical`, `p2_high`, `p3_medium`, `p4_low` to `critical`, `high`, `medium`, `low`; a manual block, an escalation, and a reopened dependency are at least `high`. `bypassDigest: true` (with channels `slack`, `email`, `in_app`) for `p1_critical` tasks, manual blocks, and escalations. | `events/task-blocked-manual.json` |
| R5.4 Actions | `actionUrls` uses only `complete`, `snooze_2h`, `approve`, `reject`, `unblock`, and `view`. `complete` and `snooze_2h` only while the task is open. `view` only when the server has `APP_BASE_URL` (D50). | `events/task-created.json` |
| R5.5 Writes, not calls | A request that changes a task writes its outbox rows in the same transaction and never calls AgentPulse; the relay delivers them afterwards. | `delivery/*.json` |

R5.6, idempotency keys. AgentPulse keeps one notification per key and tenant, so a retry never notifies twice.

| Case | Key | Fixtures |
| --- | --- | --- |
| A change with one recipient | `taskify:{tenant_id}:{task_id}:{event_type}:v{version}` | `events/task-created.json` |
| A change with several recipients | the same, then `:{user_id}` | `events/task-updated-watchers.json` |
| Due soon | `taskify:{tenant_id}:{task_id}:due_soon_24h:{due_at as Unix seconds}`, and `due_soon_1h` | `events/due-soon-and-overdue.json` |
| Overdue | `taskify:{tenant_id}:{task_id}:overdue:{YYYY-MM-DD}` | `events/due-soon-and-overdue.json` |
| Comment | `taskify:{tenant_id}:{task_id}:task.comment_added:{comment_id}`, then `:{user_id}` with several recipients | `events/comment-mention.json` |
| Dependency notice | `taskify:{tenant_id}:{task_id}:{event_type}:{blocker_id}:v{blocker_version}` for `task.dependency_added`, `task.dependency_resolved`, and `task.dependency_reopened`; `task.unblocked` uses the first format | `agents/decompose.json` (unblocked); the others in Go tests only (`internal/domain`), as no API call adds a dependency yet (G23) |
| Approval request | `taskify:{tenant_id}:{task_id}:agent.approval_requested:{job_id}` | `hitl/apply.json` |
| Escalation | `taskify:{tenant_id}:{task_id}:task.escalated:{job_id}:{user_id}` | Go tests only (`internal/agent` blocker tests) |
| Morning brief | `taskify:{tenant_id}:{job_id}:digest.morning_brief` | Go tests only (`internal/agent` digest tests): a brief runs at a fixed hour |
| Contact | `taskify:{tenant_id}:contact.update:{user_id}`, one row per person, reused | `delivery/contact.json` |
| Learning suggestion | `taskify:{tenant_id}:{job_id}:learning.suggestion:{hash}` | Go tests only (`internal/agent` learning tests): the job runs weekly |

## 6. Delivery

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R6.1 Retries | A failed send (a `5xx`, `429`, or timeout) is retried with backoff; the row stays `pending` with `attempts` counted. A non-retryable `4xx` goes to `dead_letter` at once. | `delivery/retry-then-deliver.json` |
| R6.2 Dead letter | After `max_attempts` failed sends (10 by default) the row is `dead_letter`, with `last_error`. | `delivery/dead-letter.json` |
| R6.3 Replay | An admin's `POST /admin/outbox/{id}/replay` returns a dead-letter row to `pending` with no attempts; a row in any other status is `409 not_dead_letter`. | `delivery/dead-letter.json` |

## 7. Agents and approval

R7.1, safety tiers. A Tier 1 call runs at once and is recorded in the task's agent metadata; a Tier 2 call is stored as a proposal and runs only when a person approves it. With the workspace's `require_hitl_for_all` (on by default), every call is Tier 2 except `ProposeNotificationRule`'s, whose suggestion already waits for an admin at AgentPulse (D54).

| Tool | Tier | Fixtures |
| --- | --- | --- |
| `UpdateTaskAttributes` | 1; 2 when it sets `priority` | `agents/tier1-labels.json`, `agents/tier2-priority.json` |
| `LinkDependencies` | 1 | Go tests only (`internal/agent/tools`) |
| `CreateSubtaskDAG` | 1 (a person asked for it) | `agents/decompose.json` |
| `DraftResolutionComment` | 1 | Go tests only (`internal/agent/tools`) |
| `ReassignTask`, `ShiftDueDate`, `ArchiveTask` | 2 | Go tests only (`internal/agent/tools`) |
| `ProposeNotificationRule` | 1, decided by an admin at AgentPulse | Go tests only (`internal/agent/tools`) |

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R7.2 Scope | A call may change only the job's task and its direct children, in the job's tenant; it may name other tasks of the tenant as dependencies. Arguments failing the tool's schema are refused before anything runs; the model gets one repair turn, and a second bad answer fails the job with nothing changed. | `agents/malformed.json` |
| R7.3 Loop prevention | Agent and system changes notify people but never queue agent jobs. | `agents/tier1-labels.json` |
| R7.4 Who decides | The task's assignee, its creator, or an admin; for a job with no task, an admin only. Anyone else gets `403`. | `hitl/forbidden.json` |
| R7.7 Who sees a job | A job on a task: any member of the tenant. A morning brief: the person it is for, and admins. A job about the whole tenant, such as a learning run: admins only. A job the caller may not see answers `404`, as an unknown one does, and lists leave it out without a trace in their paging (owner, 2026-11-23). | `agents/job-visibility.json`; the brief and tenant-wide kinds, which no API call creates on demand, in Go tests (`internal/api` `TestAgentJobVisibility`, the repository suites) |
| R7.5 Decisions | Repeating a decision changes nothing (`changed: false`). The opposite decision on a decided job is `409 already_decided`. A decision before the job waits for one is `409 not_awaiting_approval`. Approving when the task moved past the job's base version is `409 stale_version`, and the job fails. | `hitl/*.json` |
| R7.6 Decomposition | `POST /tasks/{id}/decompose` answers `202` with the job; while one is open, that job is returned again. Agents or the decomposer off: `409 agent_disabled`. | `agents/decompose.json`, `agents/decompose-disabled.json` |

## 8. Webhooks

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R8.1 Signature | `X-AgentPulse-Signature` is `sha256=` (optional) and the lower-case hex HMAC-SHA256 of `{X-AgentPulse-Timestamp}.{raw body}` with the shared secret. A bad or missing signature is `401`. | `webhooks/bad-signature.json` |
| R8.2 Freshness | The timestamp must be within 5 minutes of the server's clock; otherwise `401`. | `webhooks/stale-timestamp.json` |
| R8.3 Duplicates | A callback with a `callbackId` seen before in the tenant, or a byte-identical body and signature, answers `200 {"status": "duplicate"}` and changes nothing. | `webhooks/complete.json` |
| R8.4 Actions | `complete`, `snooze` (`duration`), and `unblock` need `taskId`; `approve` and `reject` need `jobId`. The matching feedback signal (`acted`, `snoozed`) is forwarded to AgentPulse once. | `webhooks/complete.json` |

## 9. Lists and pagination

| Rule | Detail | Fixtures |
| --- | --- | --- |
| R9.1 Cursors | Lists return `next_cursor`, opaque, null on the last page; `limit` is 1 to 200, 50 by default. Tasks come most recently updated first; agent jobs and outbox rows newest first. | `tasks/pagination.json` |
| R9.2 Prefix | `GET /tasks?id_prefix=` matches tasks whose ID starts with the given lower-case hex prefix, hyphens as in the canonical form. | `tasks/id-prefix.json` |

## 10. Errors

R10.1, the status of each error code:

| Status | Codes |
| --- | --- |
| 400 | `invalid_request`: unparseable body, unknown field, bad query or header |
| 401 | `unauthorized` |
| 403 | `forbidden` |
| 404 | `not_found`, also for another tenant's object and an unknown route |
| 405 | `invalid_request`: a method the route does not have |
| 409 | `version_conflict`, `already_exists`, `agent_disabled`, `stale_version`, `already_decided`, `not_awaiting_approval`, `not_dead_letter` |
| 413 | `invalid_request`: body over 1 MiB |
| 422 | `validation_failed`, `invalid_transition` |
| 428 | `version_required` |
| 429 | `rate_limited`, with `Retry-After` |
| 500 | `internal_error`, with no detail |
| 502 | `upstream_error`: AgentPulse failed the call |
| 503 | `upstream_unavailable`: AgentPulse's circuit is open |

Fixtures: `tasks/validation.json`, `tasks/update-needs-version.json`, `tasks/stale-version.json`, `auth/tokens.json`, `auth/member-forbidden.json`, `agents/decompose-disabled.json`, `hitl/*.json`, `delivery/dead-letter.json`.

A client branches on `error.code` and treats a code it does not know by its status.

## 11. Client rules

These bind every generated client (CLI, frontend, SDK):

1. Clients contain no business rules: no transition checks, no key building, no tier logic.
2. Timestamps are sent and read as RFC 3339; clients convert to local time only for display.
3. Updates always carry the version (`If-Match`), and a `409` is shown to the person, never retried silently.
4. Unknown response fields and unknown error codes are tolerated.
5. Generated code is regenerated from [openapi.yaml](openapi.yaml), never edited by hand.
