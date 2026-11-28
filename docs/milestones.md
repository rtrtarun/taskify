# Taskify Milestones

Companion documents: [features.md](features.md) (what and why) and [architecture.md](architecture.md) (how). Feature IDs (C1 to C4) refer to [features.md](features.md).

This file holds the open work only. M0 to M13 are done; their deliverables and exit criteria are in [archive/milestones-completed.md](archive/milestones-completed.md). Each milestone is broken into tasks in [implementation/README.md](implementation/README.md).

## Status (2026-11-28)

| Work | State |
| --- | --- |
| M14 (**R3**) | In progress. M14a, M14b, and the admin pages and embedded build of M14c are done. Task M14-13 (journeys, accessibility, small screens) remains |
| M15 (**R3**) | Not started |
| M0-07 | Waiting: runs the client and webhook tests against a real AgentPulse once its slices 40a to 40d ship (owner action O1, [blockers.md](blockers.md)). M2, M3, M10, M11, and M14-06 are signed off against the mock until then |

## Overview

| # | Milestone | Delivers | Source | Estimate | Depends on |
| --- | --- | --- | --- | --- | --- |
| M14 | Web frontend (**R3 complete**) | C4 | new | 10 to 14 days | M12; approvals screens need M6 |
| M15 | Python client SDK | C2 | new | 2 days | M12 |

---

## R3: Clients and portability

Design detail for this release is in [architecture.md](architecture.md) section 9.

### M14. Web frontend

**Goal:** A person who has never seen the API can manage tasks and supervise agents in a browser. Delivers feature C4.

Suggested split, each part usable on its own:

| Part | Screens | Needs |
| --- | --- | --- |
| M14a | Sign-in, task list and board, task detail, comments, create and edit, in-app notifications | M12 |
| M14b | Subtasks and dependency graph, agent activity timeline, approvals inbox, morning brief | M6, M8, M10 |
| M14c | Admin: learning suggestions, rules, agent run history, dead-letter queue | M12 |

Deliverables:

- `web/`: React and TypeScript application, with API types generated from `openapi.yaml`.
- Notification feed proxy: `GET /api/v1/notifications/in-app` and `POST /api/v1/notifications/{id}/ack`, the `notification_acks` table (migration `000005`), and enrichment with task data.
- Live updates Phase 1: polling with `ETag` and `If-None-Match` on list endpoints, and `updated_since` on the task list. Server-Sent Events are a later phase and not part of this milestone.
- Deployment: a static build for a CDN or static host with `CORS_ALLOWED_ORIGINS`, and an optional embedded build (`go:embed` behind a build tag, enabled with `SERVE_EMBEDDED_SPA=true`).
- Optimistic UI for task edits, with a clear conflict message when the server returns `409`.
- Loading, empty, and error states on every screen.
- Keyboard navigation and labelled controls on the core task flows.
- Component tests, and end-to-end browser tests for the main journeys.

Exit criteria:

- End-to-end tests pass for: create a task, edit it, comment, complete it; approve a proposal; reject a proposal; approve a learning suggestion.
- Two browser sessions editing the same task: the second save shows a conflict and offers a reload, and loses no data silently (test).
- A user in one tenant cannot load a task URL from another tenant (test).
- An agent change on the task detail page shows which agent made it and its reasoning.
- The core task flows work at phone width.
- The browser makes no request to AgentPulse; a user cannot read another user's notification feed by changing the request (test).
- With AgentPulse unreachable, the notifications panel shows an error and the task screens still work (test).
- Acknowledging a notification records it locally and results in one feedback call to AgentPulse, including when the first attempt fails (test).
- An unchanged task list returns `304` (test).
- `go build` succeeds without a frontend build present; the embedded binary serves the app and client-side routes at `/` and does not shadow `/api/` (test).

Depends on M0 confirming AgentPulse's in-app feed endpoint and its service authentication. If the feed does not exist, the notifications screen is dropped from M14a and the rest proceeds.

### M15. Python client SDK

**Goal:** Python scripts, pipelines, and agent workflows can use Taskify through a typed client. Delivers feature C2.

Deliverables:

- `sdk/python/` package, distribution name `taskify-sdk`, import name `taskify`: Pydantic models generated from `openapi.yaml`, with a thin hand-written `httpx` client on top, sync and async.
- Resources for tasks, comments, agent jobs, notifications, and admin.
- Typed exceptions per error code; `VersionConflictError` carries the current version.
- Cursor-following iterators; `If-Match` sent automatically on updates; `job.wait()` for decomposition.
- Retries with backoff for `GET` only.
- CI: `ruff`, `mypy`, `pytest`, and a check that regenerating the models produces no diff.
- A README with the Git-tag install command and usage examples.

Exit criteria:

- Tests run against a local Go server and cover create, list with pagination, update, version conflict, comment, complete, decompose and wait, approve.
- A change to `openapi.yaml` without regenerating the models fails CI.
- The token is never written to logs or exception messages (test).
- The package contains no business rules: no transition checks, no key building. Those stay in the server.

Distribution (decision 21): installed from a Git tag and the `sdk/python` subdirectory. No public wheels until v1.0.

The distribution name is `taskify-sdk`, imported as `taskify` (decision 26). Owner action, not part of this milestone: reserve `taskify-sdk` on the public index. Reserving means a public upload under the owner's account.

---

## Definition of done for every milestone

- `go test -race ./...` passes.
- New code has tests; coverage for touched packages does not fall.
- Migrations have working down scripts.
- Public endpoints and environment variables are documented in the README.
- No call to AgentPulse or to an LLM is made inside a task write handler. The endpoints that exist to talk to AgentPulse (feedback, admin proxies, the notification feed) call it with a timeout and the circuit breaker.
- From M12 onward: any change to an endpoint, event, or tool is made in `contract/` first; the conformance suite passes; and the generated clients (Go client, frontend types, Python models) are regenerated in the same change.
