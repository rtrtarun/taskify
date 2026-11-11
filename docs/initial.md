Integrating **Taskify** with your **AgentPulse (`agentic-notification`)** platform transforms Taskify from a basic CRUD starter project into an **attention-aware task orchestration engine**. Instead of firing dumb reminders that cause alert fatigue, Taskify acts as a domain event producer while AgentPulse handles deduplication, `payloadMatch` rule evaluation, attention-intelligence classification, Redis token-bucket budgets, digest batching, and closed-loop feedback learning.

---

## 1. Competitor Analysis: Task Managers & Notification Intelligence

Current task managers excel at task capture and views, but their notification engines remain largely static—relying on manual toggles or rigid time triggers that flood users when tasks slip.

| Dimension | Todoist | Linear | TickTick | Jira | **Taskify + AgentPulse (Target)** |
| --- | --- | --- | --- | --- | --- |
| **Core Focus** | Personal & team task clarity, NLP quick-add (`Ramble` voice capture) | Product & engineering issue cycles and sprints | Habits, calendar time-blocking, persistent alarms | Enterprise workflow tracking & heavy state machines | **Lightweight Go task engine + autonomous notification intelligence** |
| **Notification Routing** | Manual per-project email/push toggles | Triage inbox + Slack/GitHub event hooks | Multi-channel static time/location reminders | Complex notification schemes (high admin overhead) | **Declarative `payloadMatch` rules across channels (`webhook`, `email`, `slack`, `in_app`)** |
| **Alert Fatigue Protection** | Manual selection of activity types | Inbox grouping by issue | None (persistent ringing until dismissed) | None (infamous for email spam) | **Redis per-user attention budgets + deterministic idempotency dedup** |
| **Digest Capabilities** | Fixed Morning Overview & Evening Review on mobile | Weekly project pulse summaries | Daily agenda push | Basic batched issue update emails | **Priority-thresholded scheduled digests (low-score events auto-batched)** |
| **Adaptive Priority Scoring** | Static `p1`–`p4` user tags | Static Urgent/High/Med/Low tags | Static 4-tier priority flags | Static priority fields | **Heuristic attention classifier + offline feedback learning (bounded ±20 score tuning)** |
| **Closed-Loop Feedback** | None | Manual unsubscribe per issue | Snooze timer only | Mute issue link | **Explicit signals (`acted`, `snoozed`, `dismissed`, `muted`) with 14-day half-life decay & admin-approved tuning suggestions** |

### Strategic Wedge for Taskify + AgentPulse

1. **Zero-Config Noise Suppression:** When 20 tasks become overdue simultaneously, competitors fire 20 push alerts. Taskify + AgentPulse evaluates the user's Redis attention budget, delivers the single `p1_critical` blocker immediately, and rolls the remaining 19 into the next scheduled digest.
2. **Closed-Loop Priority Calibration:** If users consistently dismiss `task.updated` notifications for `p3_medium` tasks but immediately act on `task.blocked` notifications, AgentPulse's offline learning engine aggregates those signals (requiring at least 5 signals across at least 2 users over a 30-day window) and surfaces an admin-reviewable score adjustment suggestion.

---

## 2. Feature Roadmap: What We Need to Build

To bridge the gap between Taskify's current four-endpoint structure (`GET/POST/PUT/DELETE /tasks/`) and a production integration with AgentPulse, we need six core capability upgrades:

### A. Rich Task Domain Model

Upgrade the `Task` entity beyond `{ID, Title, Description}` to carry the metadata required by AgentPulse's heuristic classifier and `payloadMatch` rules:

* **Identity & Tenancy:** `id` (UUIDv7), `tenant_id`, `workspace_id`, `project_id`.
* **Ownership:** `creator_id`, `assignee_id`, `watcher_ids []string`.
* **State & Priority:** `status` (`todo`, `in_progress`, `blocked`, `completed`, `archived`), `priority` (`p1_critical`, `p2_high`, `p3_medium`, `p4_low`).
* **Temporal & Escalation Fields:** `due_at`, `remind_at`, `completed_at`, `last_notified_overdue_at`.
* **Context & Concurrency:** `labels []string`, `metadata map[string]any`, `version` (integer for optimistic concurrency control), `created_at`, `updated_at`.

### B. Canonical Domain Event Taxonomy

Taskify must emit structured lifecycle events mapped to AgentPulse `/notify` requests:

* `task.created` — Fired when a task is created with an assignee other than the creator.
* `task.assigned` — Fired when `assignee_id` changes.
* `task.updated` — Fired on title, description, or due-date modifications.
* `task.priority_escalated` — Fired when priority moves upward (e.g., `p3_medium` → `p1_critical`).
* `task.blocked` — Fired when status transitions to `blocked` (triggers high base attention score).
* `task.due_soon` — Fired by the scheduler 24 hours and 1 hour prior to `due_at`.
* `task.overdue` — Fired when `now` exceeds `due_at` and status is not `completed` or `archived`.
* `task.completed` — Fired to notify the creator and watchers.
* `task.comment_added` — Fired with `@mention` extraction so mentioned users receive elevated urgency.

### C. Transactional Outbox & Go SDK Relay Worker

Never call AgentPulse synchronously inside the HTTP handler for `POST /tasks` or `PUT /tasks/{id}`.

* Write the `Task` mutation and a `notification_outbox` row inside the **same PostgreSQL transaction**.
* Run a background Go worker pool that claims pending outbox rows via `SELECT ... FOR UPDATE SKIP LOCKED`, invokes the **AgentPulse Go SDK** (`Notify()`), and marks rows `dispatched` or schedules an exponential backoff retry.

### D. Deadline & Escalation Scheduler

A distributed-safe background ticker in Taskify that scans for approaching deadlines (`due_soon`) and slipped tasks (`overdue`), writing deterministic outbox events so AgentPulse's Redis deduplicator guarantees exactly one notification per escalation window.

### E. Two-Way Feedback & Action Bridge

* **Outbound Feedback Forwarding:** Expose `POST /api/v1/tasks/{id}/notifications/{notificationId}/feedback` (or capture UI actions) to forward `acted`, `dismissed`, `snoozed`, and `muted` events directly to AgentPulse's feedback endpoint.
* **Inbound Action Webhook (`POST /api/v1/webhooks/agentpulse`):** Allow interactive notifications (e.g., "Mark Done" or "Snooze 2h" clicked inside a notification channel) to callback Taskify, mutate the task state, and automatically log an `acted` or `snoozed` feedback signal in AgentPulse.

### F. Workspace Rules, Preferences & Learning Proxy

Expose admin and user endpoints in Taskify that proxy or sync with AgentPulse's Rules API (`/rules`), User Preferences (`/preferences`), Digests (`/digests`), and M38 Learning Suggestions (`/learning/suggestions`), allowing workspace admins to review and approve/reject offline score-tuning recommendations directly from Taskify.

---

## 3. High-Level Design (HLD)

### End-to-End Architecture Flow

1. **Client Tier:** CLI, Web UI, or REST consumers hit Taskify's HTTP API (`/api/v1/...`).
2. **Taskify Core Service (Go):**
* Validates requests, enforces optimistic locking (`version`), and executes ACID transactions against **PostgreSQL** (`tasks` + `notification_outbox`).
* Emits a non-blocking wake-up signal over a buffered Go channel (or Postgres `NOTIFY`) to the Outbox Relay.


3. **Outbox Relay & Deadline Scheduler (Go Worker Pool):**
* Polls `notification_outbox` in batches, enriches payloads, injects OpenTelemetry trace headers, and calls the **AgentPulse Go SDK**.


4. **AgentPulse Notification Plane (`notification-service`):**
* **Step 1 (Dedup):** Checks `idempotencyKey` in Redis.
* **Step 2 (Preferences & Rules):** Evaluates `Pipeline.applyPreferences` (with early-return guard) and matches tenant/user `payloadMatch` rules (e.g., `payload.priority == "p1_critical"` or `payload.status == "blocked"`).
* **Step 3 (Attention Intelligence & Learned Adjustments):** Computes the base heuristic priority score plus any admin-approved M38 learned adjustments (clamped within ±20).
* **Step 4 (Budget & Routing):** Checks the user's Redis attention budget. High-priority events dispatch immediately via durable delivery workers; lower-priority or budget-throttled events route to the **Digest Queue**.


5. **Feedback & Learning Loop:**
* User interaction (`acted`, `dismissed`, `snoozed`, `muted`) flows back to AgentPulse, feeding the offline score tuner (30-day lookback, 14-day exponential decay half-life) and updating Taskify task status when actionable.



---

## 4. Low-Level Design (LLD)

### A. Target Repository Structure (`taskify`)

Note: The existing README mixes `taskify` and `Taskify`. Standardize module naming to `[github.com/RONGALI-TARUN/taskify](https://github.com/RONGALI-TARUN/taskify)` and binary output to `bin/taskify`.

```text
taskify/
├── cmd/
│   └── taskify/
│       └── main.go                  # Entrypoint, graceful shutdown, dependency wiring
├── internal/
│   ├── config/
│   │   └── config.go                # Env config (DB, AgentPulse URL/APIKey, Worker tuning)
│   ├── domain/
│   │   ├── task.go                  # Task, Comment, Watcher structs & state machine rules
│   │   ├── event.go                 # Canonical event types & payload builders
│   │   └── errors.go                # Domain error definitions
│   ├── repository/
│   │   └── postgres/
│   │       ├── db.go                # pgxpool connection management & Tx helper
│   │       ├── task_repo.go         # CRUD + optimistic locking queries
│   │       └── outbox_repo.go       # Outbox insert, FOR UPDATE SKIP LOCKED claim, DLQ
│   ├── service/
│   │   ├── task_service.go          # Business logic, state transitions, outbox emission
│   │   └── feedback_service.go      # Bridges task actions <-> AgentPulse feedback & learning
│   ├── worker/
│   │   ├── outbox_relay.go          # Concurrent batch dispatcher using AgentPulse Go SDK
│   │   └── deadline_scheduler.go    # Ticker scanning due_soon and overdue tasks
│   ├── agentpulse/
│   │   └── client_adapter.go        # Wrapper around github.com/agentpulse/notification-service Go SDK
│   └── api/
│       ├── router.go                # HTTP mux, middleware (RequestID, OTel, Auth, RateLimit)
│       ├── task_handler.go          # REST handlers for tasks & comments
│       ├── webhook_handler.go       # Inbound AgentPulse action/receipt webhooks
│       └── admin_handler.go         # Proxy for AgentPulse rules, digests & M38 suggestions
├── migrations/
│   ├── 000001_init_tasks.up.sql
│   └── 000002_outbox_and_watchers.up.sql
├── docker-compose.yml               # Postgres, Redis, AgentPulse service, Taskify
└── Makefile

```

### B. PostgreSQL Database Schema (`migrations/`)

```sql
-- 000001_init_tasks.up.sql
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TYPE task_status AS ENUM ('todo', 'in_progress', 'blocked', 'completed', 'archived');
CREATE TYPE task_priority AS ENUM ('p1_critical', 'p2_high', 'p3_medium', 'p4_low');

CREATE TABLE tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    title VARCHAR(255) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status task_status NOT NULL DEFAULT 'todo',
    priority task_priority NOT NULL DEFAULT 'p3_medium',
    creator_id VARCHAR(128) NOT NULL,
    assignee_id VARCHAR(128),
    labels TEXT[] NOT NULL DEFAULT '{}',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    due_at TIMESTAMPTZ,
    remind_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    last_due_soon_notified_at TIMESTAMPTZ,
    last_overdue_notified_at TIMESTAMPTZ,
    version INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tasks_tenant_assignee_status ON tasks (tenant_id, assignee_id, status);
CREATE INDEX idx_tasks_due_scan ON tasks (due_at, status)
    WHERE status NOT IN ('completed', 'archived') AND due_at IS NOT NULL;

CREATE TABLE task_watchers (
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    user_id VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, user_id)
);

CREATE TABLE task_comments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    author_id VARCHAR(128) NOT NULL,
    body TEXT NOT NULL,
    mentions TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 000002_outbox_and_watchers.up.sql
CREATE TYPE outbox_status AS ENUM ('pending', 'processing', 'dispatched', 'dead_letter');

CREATE TABLE notification_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    aggregate_id UUID NOT NULL,
    event_type VARCHAR(64) NOT NULL,
    idempotency_key VARCHAR(192) NOT NULL UNIQUE,
    payload JSONB NOT NULL,
    trace_context JSONB NOT NULL DEFAULT '{}'::jsonb,
    status outbox_status NOT NULL DEFAULT 'pending',
    attempts INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 10,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dispatched_at TIMESTAMPTZ
);

CREATE INDEX idx_outbox_poll ON notification_outbox (next_attempt_at, created_at)
    WHERE status = 'pending';

```

### C. API Endpoints & AgentPulse Contract Mapping

#### 1. Taskify REST Endpoints

* `POST /api/v1/tasks` — Create task (writes `tasks` + `notification_outbox` in one Tx).
* `GET /api/v1/tasks` — List tasks with cursor pagination (`?assignee_id=...&status=...&priority=...&limit=50&cursor=...`). Keeps backward-compatible alias at `/tasks/` for existing CLI/scripts.
* `GET /api/v1/tasks/{id}` — Fetch single task with watchers and recent comments.
* `PUT /api/v1/tasks/{id}` & `PATCH /api/v1/tasks/{id}` — Update task fields (requires `If-Match` or `version` field; emits `task.updated`, `task.assigned`, `task.priority_escalated`, or `task.blocked`).
* `POST /api/v1/tasks/{id}/complete` — Transition to `completed` (emits `task.completed` and records `acted` feedback if triggered from a notification context).
* `DELETE /api/v1/tasks/{id}` — Soft-archive or delete task.
* `POST /api/v1/tasks/{id}/comments` — Add comment, parse `@username` mentions, emit `task.comment_added`.
* `POST /api/v1/tasks/{id}/feedback` — Forward explicit user notification feedback (`acted`, `dismissed`, `snoozed`, `muted`) to AgentPulse.
* `POST /api/v1/webhooks/agentpulse` — Receive bidirectional callbacks from AgentPulse.
* `GET /api/v1/admin/notification-suggestions` & `POST /api/v1/admin/notification-suggestions/{id}/decision` — Surface AgentPulse M38 learning suggestions (`approve` / `reject`) in Taskify.

#### 2. Canonical Outbox-to-AgentPulse `/notify` Payload

When a task transitions to `blocked` on a `p1_critical` item, Taskify's Outbox Relay sends the following structure to AgentPulse via the Go SDK:

```json
{
  "idempotencyKey": "taskify:tenant_acme:0192f4a0-7c11-7000-8000-1a2b3c4d5e6f:task.blocked:v4",
  "tenantId": "tenant_acme",
  "userId": "usr_assignee_42",
  "source": "taskify",
  "eventType": "task.blocked",
  "title": "[BLOCKED] Production DB migration script failing on staging",
  "body": "Task #0192f4a0 was marked as BLOCKED by usr_dev_19. Priority: P1 Critical.",
  "priorityHint": "critical",
  "channels": ["slack", "email", "in_app"],
  "payload": {
    "taskId": "0192f4a0-7c11-7000-8000-1a2b3c4d5e6f",
    "workspaceId": "ws_core_eng",
    "status": "blocked",
    "previousStatus": "in_progress",
    "priority": "p1_critical",
    "dueAt": "2026-11-12T14:00:00Z",
    "labels": ["database", "release-blocker"],
    "actorId": "usr_dev_19",
    "version": 4,
    "actionUrls": {
      "acknowledge": "https://taskify.internal/api/v1/tasks/0192f4a0-7c11-7000-8000-1a2b3c4d5e6f/feedback?signal=acted",
      "snooze_2h": "https://taskify.internal/api/v1/tasks/0192f4a0-7c11-7000-8000-1a2b3c4d5e6f/feedback?signal=snoozed&duration=2h"
    }
  }
}

```

#### 3. Default AgentPulse `payloadMatch` Rules Bootstrapped by Taskify

On workspace creation, Taskify registers three baseline rules with AgentPulse's `/rules` API:

1. **Critical / Blocked Immediate Escalation:**
* `payloadMatch`: `{"priority": "p1_critical"}` OR `{"status": "blocked"}`
* `channels`: `["slack", "email", "in_app"]`, bypass digest.


2. **Direct Assignment & Mentions:**
* `eventType`: `task.assigned` OR `task.comment_added` (where `isMentioned == true`)
* `channels`: `["in_app", "email"]`.


3. **Low-Priority Task Chime (Digest Eligible):**
* `payloadMatch`: `{"priority": "p4_low"}`
* `channels`: `["digest"]` (batches into user's configured daily digest window).



---

## 5. Cross-Cutting Concerns: Scalability, Performance & Reliability

### Performance Specifications & Latency Budgets

* **Task CRUD Write Path (P95 under 15 ms, P99 under 30 ms):** Because HTTP handlers only write to PostgreSQL (`tasks` + `notification_outbox`) and never make blocking network calls to AgentPulse, Taskify API latency stays completely insulated from notification downstream latency or transient outages.
* **Task Read Path (P95 under 8 ms):** Composite B-Tree index `(tenant_id, assignee_id, status)` with keyset cursor pagination (`WHERE (updated_at, id) ... LIMIT 50`) eliminates `OFFSET` scan degradation at millions of rows.
* **Outbox Dispatch Latency (P95 under 150 ms, P99 under 400 ms):** The Outbox Relay listens on a local Go wake-up channel (for single-node instant trigger) plus a 200 ms ticker claiming batches of 100 rows via `FOR UPDATE SKIP LOCKED`.

### Scalability & Concurrency Architecture

* **Stateless Horizontal Scaling:** Multiple Taskify replicas can run concurrently behind an L7 load balancer.
* **Lock-Free Worker Coordination:** Using PostgreSQL `FOR UPDATE SKIP LOCKED` on `notification_outbox` and `tasks` (during deadline scans) allows 1 to $N$ Taskify worker pods to process events in parallel without distributed lock managers or duplicate claims.
* **Bounded Worker Pool & Backpressure:** Each Taskify replica runs a configurable worker pool (default `OUTBOX_WORKERS=8`, `BATCH_SIZE=100`). Outbound HTTP connections to AgentPulse use a tuned `http.Transport` (`MaxIdleConnsPerHost: 64`, `IdleConnTimeout: 90s`).

### Durability, Idempotency & Fault Tolerance

* **Deterministic Idempotency Keys:** Every event generates a collision-free key:
* State mutations: `taskify:{tenant_id}:{task_id}:{event_type}:v{version}`
* Scheduled reminders: `taskify:{tenant_id}:{task_id}:due_soon_24h` and `taskify:{tenant_id}:{task_id}:overdue:{YYYY-MM-DD}`
Even if an Outbox Relay crashes after AgentPulse commits the notification but before Taskify marks the row `dispatched`, the retry carries the exact same `idempotencyKey` and is safely deduplicated by AgentPulse's Redis layer.


* **Exponential Backoff with Full Jitter & Circuit Breaking:**
* Failed dispatches increment `attempts` and compute `next_attempt_at = now() + random(0, min(60s, 500ms * 2^attempts))`.
* After 10 failed attempts, the row transitions to `status = 'dead_letter'` and increments the `taskify_outbox_dlq_total` Prometheus counter.
* A circuit breaker trips to `Open` for 10 seconds if AgentPulse returns 5xx errors on more than 50% of requests over a rolling 20-request window.


* **Outbox Table Retention:** A daily cleanup job deletes `dispatched` rows older than 7 days in batches of 5,000 to prevent table bloat and keep the partial index `idx_outbox_poll` tiny in RAM.

### Observability & Security

* **Distributed Tracing (OpenTelemetry):** The HTTP handler serializes the active W3C `traceparent` into `notification_outbox.trace_context`. The Outbox Relay restores that span context (`otel.GetTextMapPropagator().Extract(...)`) when calling the AgentPulse Go SDK so a single trace spans `Client → Taskify API → Postgres Outbox → Outbox Worker → AgentPulse /notify → Redis/Delivery`.
* **Prometheus Metrics (`/metrics`):**
* `taskify_http_request_duration_seconds` (histogram by route, method, status)
* `taskify_outbox_pending_rows` (gauge)
* `taskify_outbox_dispatch_duration_seconds` (histogram)
* `taskify_outbox_dlq_total` (counter)
* `taskify_feedback_forwarded_total` (counter by signal: `acted`, `dismissed`, `snoozed`, `muted`)


* **Webhook Security:** Inbound requests to `POST /api/v1/webhooks/agentpulse` verify `X-AgentPulse-Signature` using HMAC-SHA256 and a timestamp replay window of 5 minutes.

---

## 6. Master Autonomous Prompt for Claude

Copy and paste the prompt below directly into Claude to execute the full upgrade and integration across `taskify` and `agentic-notification`:

```markdown
You are a Staff Distributed Systems Engineer working in Go. Your objective is to upgrade the `taskify` repository from a basic in-memory/simple CRUD app into a production-grade, multi-tenant task orchestration service deeply integrated with our existing `agentic-notification` platform (`github.com/agentpulse/notification-service`).

### Phase 0: Repository Reconnaissance & Alignment
1. Inspect the current `taskify` workspace structure, `go.mod`, handlers, and models.
2. If the `agentic-notification` repository or its Go SDK (`sdk/go` or `pkg/client`) is present in the parent workspace or accessible via module/HTTP contract, inspect its exact `/notify`, `/rules`, `/preferences`, `/feedback`, `/digests`, and `/learning/suggestions` (contract `14-learning-suggestions.json`) schemas so our client adapter matches 100% of field names and invariants.
3. Fix the naming mismatch in `taskify` (standardize binary name, `go.mod` module path, and README instructions while keeping legacy `/tasks/` routes aliased to `/api/v1/tasks` for backward compatibility).

### Phase 1: Database Schema & Domain Layer (Milestones T1-T2)
1. Add PostgreSQL migrations (`migrations/000001_init_tasks.up.sql` and `000002_outbox.up.sql`):
   - `tasks` table: `id` (UUID), `tenant_id`, `workspace_id`, `title`, `description`, `status` (`todo`, `in_progress`, `blocked`, `completed`, `archived`), `priority` (`p1_critical`, `p2_high`, `p3_medium`, `p4_low`), `creator_id`, `assignee_id`, `labels` (`TEXT[]`), `metadata` (`JSONB`), `due_at`, `remind_at`, `completed_at`, `last_due_soon_notified_at`, `last_overdue_notified_at`, `version` (INT for optimistic locking), `created_at`, `updated_at`.
   - `task_watchers` and `task_comments` tables (with `@mention` extraction).
   - `notification_outbox` table: `id`, `tenant_id`, `aggregate_id`, `event_type`, `idempotency_key` (UNIQUE), `payload` (JSONB), `trace_context` (JSONB), `status` (`pending`, `processing`, `dispatched`, `dead_letter`), `attempts`, `max_attempts`, `next_attempt_at`, `last_error`, `created_at`, `dispatched_at`.
   - Create composite and partial indexes (`idx_tasks_tenant_assignee_status`, `idx_tasks_due_scan`, `idx_outbox_poll`).
2. Implement `internal/domain/task.go` and `internal/domain/event.go`:
   - Enforce state transition validation and optimistic concurrency increments (`version + 1`).
   - Generate canonical domain events: `task.created`, `task.assigned`, `task.updated`, `task.priority_escalated`, `task.blocked`, `task.due_soon`, `task.overdue`, `task.completed`, `task.comment_added`.
   - Build deterministic idempotency keys:
     - Mutations: `taskify:{tenant_id}:{task_id}:{event_type}:v{version}`
     - Deadlines: `taskify:{tenant_id}:{task_id}:due_soon_24h` and `taskify:{tenant_id}:{task_id}:overdue:{YYYY-MM-DD}`.

### Phase 2: Transactional Outbox, AgentPulse SDK Adapter & Deadline Scheduler (Milestone T3)
1. In `internal/repository/postgres/`, implement transactional task persistence where every state mutation inserts the corresponding `notification_outbox` record inside the exact same database transaction (`pgx.Tx`). Also provide an in-memory repository fallback implementation with identical interfaces so unit tests can run without external Docker dependencies when needed.
2. In `internal/agentpulse/client.go`, build the client adapter integrating with `agentic-notification`:
   - Support `Notify(ctx, req)`, `CreateRule(ctx, rule)`, `RecordFeedback(ctx, fb)`, `ListLearningSuggestions(ctx, tenantID)`, and `ReviewLearningSuggestion(ctx, suggestionID, decision)`.
   - Include a circuit breaker (trips after >50% 5xx errors in 20 requests, 10s cooldown) and W3C `traceparent` header injection.
3. In `internal/worker/outbox_relay.go`, implement a concurrent worker pool:
   - Poll `notification_outbox` using `SELECT ... FOR UPDATE SKIP LOCKED LIMIT $1`.
   - Dispatch events to AgentPulse, mark `dispatched` on success, or apply exponential backoff with full jitter (`base=500ms`, `max=60s`, `max_attempts=10`) transitioning to `dead_letter` on exhaustion.
4. In `internal/worker/deadline_scheduler.go`, implement a background ticker that scans for tasks due within 24h (`task.due_soon`) and overdue tasks (`task.overdue`), atomically updating `last_due_soon_notified_at` / `last_overdue_notified_at` and writing outbox events in the same transaction.

### Phase 3: REST API, Feedback Loop & M38 Learning Bridge (Milestone T4)
1. Implement REST endpoints under `/api/v1/tasks` (plus backward-compatible `/tasks/` routes):
   - `POST /api/v1/tasks`, `GET /api/v1/tasks` (cursor-paginated), `GET /api/v1/tasks/{id}`, `PUT /api/v1/tasks/{id}`, `PATCH /api/v1/tasks/{id}`, `DELETE /api/v1/tasks/{id}`, `POST /api/v1/tasks/{id}/complete`, `POST /api/v1/tasks/{id}/comments`.
2. Implement the Feedback & Bidirectional Webhook endpoints:
   - `POST /api/v1/tasks/{id}/feedback`: Accepts `acted`, `dismissed`, `snoozed`, `muted` and forwards the signal to AgentPulse's feedback endpoint so the M38 offline score tuner (30-day lookback, 14-day half-life, >=5 signals from >=2 users, ±20 clamp) receives real task interaction data.
   - `POST /api/v1/webhooks/agentpulse`: Validates `X-AgentPulse-Signature` (HMAC-SHA256), handles interactive action callbacks (e.g., completing a task or snoozing a deadline from a notification), and updates task state.
   - `POST /api/v1/workspaces/{id}/bootstrap-rules`: Provisions default `payloadMatch` rules in AgentPulse for `p1_critical`/`blocked` immediate escalation vs `p4_low` digest batching.
   - `GET /api/v1/admin/learning/suggestions` & `POST /api/v1/admin/learning/suggestions/{id}/decision`: Proxy endpoints for workspace admins to inspect and approve/reject AgentPulse M38 score tuning suggestions.

### Phase 4: Observability, Verification & Documentation (Milestone T5)
1. Expose `/healthz`, `/readyz`, and `/metrics` (Prometheus metrics for HTTP latency, outbox pending depth, dispatch duration, DLQ count, and feedback signals).
2. Write comprehensive unit and integration tests (>90% coverage across `domain`, `service`, `worker`, and `api`):
   - Test atomic rollback (if outbox insert fails, task creation rolls back).
   - Test concurrent Outbox Relay workers verifying zero duplicate dispatches.
   - Test retry backoff, circuit breaker state transitions, and DLQ promotion after 10 failures.
   - Test end-to-end flow against a mock/test AgentPulse server verifying `/notify` contract, `idempotencyKey` determinism, `/feedback` forwarding, and M38 learning suggestion admin approval.
3. Update `README.md` with the new architecture diagram, API documentation, `docker-compose.yml` setup (running Postgres + Redis + AgentPulse + Taskify), and benchmark instructions.

Execute step-by-step. Run `go test -race ./...` after each milestone and ensure zero warnings or race conditions before proceeding.

```

---

## Interactive Architecture Explorer

Use the interactive diagram below to inspect each component in the **Taskify + AgentPulse** integration and compare how the **Asynchronous Transactional Outbox** pattern behaves against a naive **Synchronous Direct Call** setup under high task volume or downstream notification slowdowns.