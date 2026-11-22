# Taskify Architecture

Companion documents: [features.md](features.md) (what and why), [milestones.md](milestones.md) (delivery order), and [implementation/README.md](implementation/README.md) (task-level plan). Source material, now archived: [initial.md](archive/initial.md) and [agentic.md](archive/agentic.md).

Diagrams are written in Mermaid. They render on GitHub and in VS Code with a Mermaid preview extension.

Contents:

1. [Scope and current state](#1-scope-and-current-state)
2. [High-level design](#2-high-level-design)
3. [Flows](#3-flows)
4. [Data model](#4-data-model)
5. [Low-level design](#5-low-level-design)
6. [Cross-cutting concerns](#6-cross-cutting-concerns)
7. [Where this document departs from the source documents](#7-where-this-document-departs-from-the-source-documents)
8. [Decisions](#8-decisions)
9. [Clients and portability: contract, Python, CLI, web frontend](#9-clients-and-portability)

Sections 2 to 6 describe the Go service. Section 9 describes what is built around it; it is placed last so that existing section numbers stay stable.

---

## 1. Scope and current state

**Target:** a multi-tenant Go task service that emits task lifecycle events to AgentPulse through a transactional outbox, and runs background LLM agents that enrich, decompose, and unblock tasks under human-in-the-loop controls. Around it sit a language-neutral contract, a CLI, a web frontend, and a Python client SDK (section 9). There is one server, in Go.

**Today (2026-11-22):** release R1 is built and released (v0.1.0 to v0.3.0): the task service, transactional outbox, relay, scheduler, REST API, webhooks, and observability in sections 2 to 6. R2 is complete: the agent runtime (M5), the human-in-the-loop bridge (M6), the triage, decomposer, blocker, digest, and learning agents (M7 to M11), and the AgentPulse `/v1/` alignment (AP), with the departures listed in [implementation/README.md](implementation/README.md) section 8. Everything in section 9 (R3) is still to be built. Progress per milestone is in [milestones.md](milestones.md).

**AgentPulse contract: agreed, not yet run against AgentPulse.** The AgentPulse repository was read on 2026-11-22. It did not match the contract this document was first written against, and the owner chose to extend AgentPulse to Taskify's design (decision 27). The contract in section 5.5 is the one agreed with AgentPulse's milestone 40, recorded in [blockers.md](blockers.md). It has been checked against AgentPulse's plan for that milestone, not against running AgentPulse code: Taskify's client has only run against the mock in `tests/mockagentpulse`, which enforces the agreed rules. Task M0-07 runs the client and webhook tests against a real AgentPulse once slices 40a to 40d ship.

---

## 2. High-level design

### 2.1 System context

```mermaid
flowchart LR
    user["Users and scripts"]
    admin["Workspace admin"]
    cli["taskctl CLI"]
    web["Web frontend"]
    pysdk["Python SDK"]
    subgraph taskify["Taskify server"]
        api["HTTP API"]
        workers["Background workers"]
    end
    pg[("PostgreSQL")]
    ap["AgentPulse notification-service"]
    llm["LLM endpoint: Ollama, vLLM, or hosted API"]
    channels["Slack, email, in-app, webhook"]

    user --> cli & web & pysdk
    pysdk -->|"REST /api/v1"| api
    user -->|"REST /api/v1"| api
    admin --> web
    cli -->|"REST /api/v1"| api
    web -->|"REST /api/v1"| api
    api --> pg
    workers --> pg
    workers -->|"notify, feedback, rules"| ap
    workers -->|"chat completion with tools"| llm
    ap -->|"deliver"| channels
    channels -->|"user clicks action"| ap
    ap -->|"signed webhook"| api
```

Taskify owns task data and decides **what happened**. AgentPulse decides **who is told, when, and how**. The LLM proposes actions; Taskify's Go code decides whether and how they run.

### 2.2 Containers and components

One Go binary runs the API and all workers. Workers coordinate through PostgreSQL row locks, so any number of replicas can run.

```mermaid
flowchart TB
    subgraph proc["taskify process"]
        direction TB
        subgraph apiLayer["API layer"]
            router["Router and middleware"]
            taskH["Task handlers"]
            hookH["Webhook handler"]
            adminH["Admin handlers"]
            agentH["Agent job handlers"]
        end
        subgraph svcLayer["Service layer"]
            taskSvc["TaskService"]
            fbSvc["FeedbackService"]
            agentSvc["AgentJobService"]
        end
        subgraph workerLayer["Workers"]
            relay["Outbox relay"]
            sched["Deadline scheduler"]
            agentRt["Agent runtime"]
            cleanup["Outbox cleanup"]
        end
        subgraph agentPkg["Agent package"]
            agents["Triage, Decomposer, Blocker, Digest, Learning"]
            registry["Tool registry"]
            llmClient["LLM client"]
        end
        subgraph adapters["Adapters"]
            repo["Postgres repositories"]
            apClient["AgentPulse client with circuit breaker"]
        end
    end
    pg[("PostgreSQL")]
    ap["AgentPulse"]
    llm["LLM endpoint"]

    router --> taskH & hookH & adminH & agentH
    taskH --> taskSvc
    hookH --> taskSvc & fbSvc & agentSvc
    adminH --> apClient
    agentH --> agentSvc
    taskSvc --> repo
    fbSvc --> apClient
    agentSvc --> repo
    relay --> repo & apClient
    sched --> repo
    cleanup --> repo
    agentRt --> repo & agents
    agents --> registry & llmClient
    registry --> taskSvc
    repo --> pg
    apClient --> ap
    llmClient --> llm
```

### 2.3 Component responsibilities

| Component | Responsibility | Never does |
| --- | --- | --- |
| API layer | Parse, authenticate, validate, map errors to HTTP status | Call AgentPulse or the LLM for task writes |
| `TaskService` | Apply business rules, run the transaction that writes task, outbox, and agent job rows | Network I/O |
| `FeedbackService` | Forward feedback signals to AgentPulse | Mutate tasks |
| `AgentJobService` | Apply or dismiss Tier 2 proposals | Call the LLM |
| Outbox relay | Claim pending outbox rows, call AgentPulse, record result | Create events |
| Deadline scheduler | Find due-soon, overdue, and stale tasks; write reminder events; queue blocker investigations, the daily morning briefs, and the weekly learning jobs | Call AgentPulse directly |
| Agent runtime | Claim agent jobs, run the reasoning loop, record history | Mutate tasks except through tools |
| Tool registry | Validate and execute typed tool calls via `TaskService` | Accept tenant or scope from model output |
| LLM client | OpenAI-compatible chat completion with tool calling, timeout, retry | Hold business logic |
| AgentPulse client | Typed calls, circuit breaker, trace header injection | Retry on its own (the relay owns retry) |
| Repositories | SQL, transactions, optimistic locking, `SKIP LOCKED` claims | Business rules |

### 2.4 Design principles

1. **No remote calls on the write path.** HTTP handlers write to PostgreSQL only. This keeps API latency independent of AgentPulse and LLM latency.
2. **One transaction per state change.** Task row, outbox rows, and agent job rows commit together or not at all.
3. **At-least-once delivery, deduplicated downstream.** The relay may send an event twice; the deterministic idempotency key makes that harmless.
4. **PostgreSQL is the only coordinator.** `FOR UPDATE SKIP LOCKED` replaces a distributed lock manager or message broker.
5. **Models propose, Go disposes.** The LLM emits tool calls. Typed Go code validates, tiers, and executes them.
6. **Humans approve what is hard to undo.**

---

## 3. Flows

### 3.1 Task write with transactional outbox

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant H as Task handler
    participant S as TaskService
    participant DB as PostgreSQL
    participant W as Wake-up channel

    C->>H: PATCH /api/v1/tasks/ID with If-Match version
    H->>S: UpdateTask(cmd)
    S->>DB: BEGIN
    S->>DB: SELECT task FOR UPDATE
    S->>S: validate transition, diff fields, derive events
    S->>DB: UPDATE tasks SET ..., version = version + 1 WHERE id AND version
    alt zero rows updated
        S->>DB: ROLLBACK
        S-->>H: ErrVersionConflict
        H-->>C: 409 Conflict
    else updated
        S->>DB: INSERT notification_outbox, one row per event and recipient
        S->>DB: INSERT agent_jobs if a human-authored event matches an agent trigger and workspace agent_settings allow it
        S->>DB: COMMIT
        S->>W: non-blocking signal
        S-->>H: updated task
        H-->>C: 200 OK with new version
    end
```

### 3.2 Outbox relay dispatch

```mermaid
sequenceDiagram
    autonumber
    participant R as Relay worker
    participant DB as PostgreSQL
    participant CB as Circuit breaker
    participant AP as AgentPulse

    loop on wake-up signal or every 200 ms
        R->>DB: claim batch: pending and next_attempt_at due, FOR UPDATE SKIP LOCKED, set processing
        DB-->>R: up to 100 rows
        par for each row, bounded by worker count
            R->>CB: allow?
            alt breaker open
                CB-->>R: rejected
                R->>DB: set pending, next_attempt_at after cooldown, attempts unchanged
            else breaker closed
                R->>AP: POST /v1/notify with idempotencyKey and traceparent, or PUT the contact for a contact.update row
                alt 2xx, or duplicate acknowledged
                    R->>DB: set dispatched, dispatched_at = now
                else 4xx other than 429
                    R->>DB: set dead_letter with last_error
                else 5xx, 429, or timeout
                    R->>DB: attempts + 1
                    alt attempts reached max_attempts
                        R->>DB: set dead_letter
                    else
                        R->>DB: set pending, next_attempt_at = now + random(0, min(60s, 500ms * 2^attempts))
                    end
                end
            end
        end
    end
```

A reaper returns rows left in `processing` for longer than a lease period (for example 60 seconds) to `pending`. This covers a relay that crashes between claim and result.

### 3.3 Deadline scheduler

```mermaid
sequenceDiagram
    autonumber
    participant T as Scheduler tick
    participant DB as PostgreSQL

    T->>DB: BEGIN
    T->>DB: SELECT open tasks with due_at within 24h and not yet notified, FOR UPDATE SKIP LOCKED LIMIT n
    T->>DB: INSERT outbox task.due_soon, ON CONFLICT (idempotency_key) DO NOTHING
    T->>DB: UPDATE tasks SET last_due_soon_notified_at = now
    T->>DB: COMMIT
    T->>DB: BEGIN
    T->>DB: SELECT open tasks with due_at in the past and last_overdue_notified_at before today, FOR UPDATE SKIP LOCKED LIMIT n
    T->>DB: INSERT outbox task.overdue keyed by date, ON CONFLICT DO NOTHING
    T->>DB: UPDATE tasks SET last_overdue_notified_at = now
    T->>DB: COMMIT
    T->>DB: BEGIN
    T->>DB: SELECT in_progress tasks with no edit or comment for 48h and no investigation today, FOR UPDATE SKIP LOCKED LIMIT n
    T->>DB: INSERT agent_jobs blocker_investigator, one per task per UTC day
    T->>DB: COMMIT
    opt from DIGEST_BRIEF_HOUR_UTC until the day's scan completes
        T->>DB: list people with open work or a pending approval and no brief job today
        T->>DB: INSERT agent_jobs digest, one per person per UTC day
    end
    opt from the first tick of each ISO week until the week's scan completes
        T->>DB: list tenants with a workspace that allows the learning agent and no job this week
        T->>DB: INSERT agent_jobs learning, one per tenant per ISO week
    end
```

Two protections make this safe across replicas: `SKIP LOCKED` prevents two schedulers from taking the same task, and the unique `idempotency_key` prevents a second insert if they somehow do. Each scan runs in batches of n (100) until a batch comes back short.

The same tick queues three kinds of agent job. Each is checked against the workspace settings by the query that finds it, is queued whatever `AGENTS_ENABLED` is (as a task change's jobs are), and is kept unique by its job dedup key (section 4.4), so overlapping scans on two replicas queue it once.

| Job | Who or what | When | Details |
| --- | --- | --- | --- |
| Stale investigation (`blocker_investigator`) | An `in_progress` task with neither an edit nor a comment, from anyone, in 48 hours (G12), in a workspace that allows the blocker agent | Every tick, at most once per task per UTC day | D33 |
| Morning brief (`digest`, no task) | Each person who is the assignee of a `todo` or `in_progress` task, or the owner of a task with a job awaiting approval, in a workspace that allows the digest agent | From `DIGEST_BRIEF_HOUR_UTC` (default 08:00 UTC; no per-user time zone), once per person per UTC day, covering all their workspaces | D40; the agent then skips a person with nothing to report (D39) |
| Learning (`learning`, no task) | Each tenant with a workspace that allows the learning agent | From the first tick of each ISO week (Monday 00:00 UTC), once per tenant per week | D55 |

Briefs and learning jobs take no row locks. A replica remembers the day's brief scan, and the week's learning scan, once one runs to the end, so later ticks skip it; a batch that another replica has just queued ends the scan until the next tick.

### 3.4 Notification pipeline inside AgentPulse

Shown for orientation. Taskify does not implement this.

```mermaid
flowchart LR
    n["/v1/notify request"] --> d{"idempotencyKey seen in Redis?"}
    d -->|yes| drop["Acknowledge, do nothing"]
    d -->|no| p["Apply user preferences"]
    p --> r["Match payloadMatch rules"]
    r --> s["Attention score = heuristic + approved learned adjustment, clamped to plus or minus 20"]
    s --> b{"Score high and budget available?"}
    b -->|yes| now["Deliver now: Slack, email, in-app"]
    b -->|no| dig["Digest queue"]
```

### 3.5 Feedback and inbound action webhook

```mermaid
sequenceDiagram
    autonumber
    participant U as User
    participant CH as Slack or email
    participant AP as AgentPulse
    participant H as Webhook handler
    participant S as TaskService
    participant F as FeedbackService

    U->>CH: clicks Mark done
    CH->>AP: action callback
    AP->>H: POST /api/v1/webhooks/agentpulse with X-AgentPulse-Signature
    H->>H: verify HMAC-SHA256, reject if timestamp older than 5 min
    alt invalid
        H-->>AP: 401
    else valid
        H->>S: CompleteTask(taskID, actor)
        S-->>H: ok, task.completed queued in outbox
        H->>F: RecordFeedback(acted)
        F->>AP: POST /v1/feedback
        H-->>AP: 200
    end
```

The direct path, `POST /api/v1/tasks/{id}/feedback`, skips the webhook and calls `FeedbackService` for signals raised inside a Taskify client.

### 3.6 Agent job loop

```mermaid
sequenceDiagram
    autonumber
    participant RT as Agent runtime
    participant DB as PostgreSQL
    participant A as Agent
    participant L as LLM
    participant TR as Tool registry
    participant S as TaskService

    RT->>DB: claim pending agent_jobs, FOR UPDATE SKIP LOCKED, set running
    RT->>A: Run(job)
    A->>DB: load task, comments, dependencies, similar tasks
    A->>A: build messages and the tool list allowed for this agent
    loop at most MaxTurns
        A->>L: chat completion with tools, 30 s timeout
        alt HTTP 429 or 503
            A->>L: retry with exponential backoff
        end
        L-->>A: tool calls or final message
        A->>DB: INSERT agent_run_history
        alt output is not valid JSON or fails schema
            A->>L: one repair turn carrying the validation error
        else valid tool call
            A->>TR: Dispatch(tenant and task from job, name, args)
            alt Tier 1
                TR->>S: typed mutation with expected version, actor = agent
                S-->>TR: result or ErrVersionConflict
                TR-->>A: tool result
            else Tier 2
                TR-->>A: recorded as proposal, not executed
            end
        end
    end
    alt proposals exist
        RT->>DB: save proposed_actions, set awaiting_approval, INSERT outbox agent.approval_requested
    else no proposals
        RT->>DB: save execution_result, set completed
    end
```

On `ErrVersionConflict` the job is returned to `pending` so the next run sees the human's edit. On an unrecoverable error the job is set to `failed` with `error_message`. A failed job never leaves partial Tier 1 writes from the same tool call, because each tool call is one transaction.

### 3.7 Human-in-the-loop approval

```mermaid
sequenceDiagram
    autonumber
    participant RT as Agent runtime
    participant DB as PostgreSQL
    participant R as Outbox relay
    participant AP as AgentPulse
    participant U as User
    participant H as Agent job handler
    participant J as AgentJobService

    RT->>DB: job awaiting_approval, outbox agent.approval_requested
    R->>AP: /v1/notify with actionUrls approve and reject
    AP->>U: interactive notification
    U->>AP: clicks Approve
    AP->>H: signed callback, routed to apply
    H->>J: Apply(jobID, decidedBy)
    J->>DB: BEGIN, SELECT job FOR UPDATE
    alt job not awaiting_approval
        J-->>H: no-op, already decided
    else task version differs from base_task_version
        J->>DB: set failed, error_message = stale_version, COMMIT
        J-->>H: 409
    else current
        J->>DB: apply proposed actions, write outbox events
        J->>DB: set completed, decided_by, decided_at, COMMIT
        J-->>H: 200
    end
    H->>AP: POST /feedback acted
```

Reject follows the same path to `dismissed`, changes no task data, and records `dismissed` feedback.

### 3.8 Task decomposition

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant H as Task handler
    participant DB as PostgreSQL
    participant A as DecomposerAgent
    participant L as LLM
    participant T as CreateSubtaskDAG tool

    C->>H: POST /api/v1/tasks/ID/decompose
    H->>DB: INSERT agent_jobs type decomposer
    H-->>C: 202 Accepted with job id
    A->>L: parent task plus instruction to produce subtasks with depends_on
    L-->>A: CreateSubtaskDAG tool call
    A->>T: Execute
    T->>T: resolve depends_on titles to siblings, reject unknown titles
    T->>T: topological sort, reject cycles
    T->>DB: BEGIN
    T->>DB: INSERT child tasks with parent_id, those with blockers as blocked with reason dependency
    T->>DB: INSERT task_dependencies rows, one per depends_on link
    T->>DB: INSERT outbox task.created only for children with no blockers
    T->>DB: COMMIT
    Note over DB: later, when a blocker completes
    DB->>DB: TaskService finds children whose blockers are all completed or archived and writes task.unblocked to outbox
```

---

## 4. Data model

### 4.1 Entity relationship diagram

```mermaid
erDiagram
    TASKS ||--o{ TASKS : "parent_id"
    TASKS ||--o{ TASK_WATCHERS : "watched by"
    TASKS ||--o{ TASK_COMMENTS : "has"
    TASKS ||--o{ NOTIFICATION_OUTBOX : "aggregate_id"
    TASKS ||--o{ AGENT_JOBS : "subject of"
    AGENT_JOBS ||--o{ AGENT_RUN_HISTORY : "records"
    AGENT_JOBS ||--o{ LEARNING_SUGGESTIONS : "submits"
    TASKS ||--o{ TASK_DEPENDENCIES : "is blocked via task_id"
    TASKS ||--o{ TASK_DEPENDENCIES : "blocks via depends_on_task_id"
    WORKSPACES ||--o{ TASKS : "contains"

    WORKSPACES {
        varchar tenant_id PK
        varchar id PK
        varchar name
        jsonb agent_settings "R2"
        timestamptz created_at
        timestamptz updated_at
    }
    TASK_DEPENDENCIES {
        uuid task_id PK, FK
        uuid depends_on_task_id PK, FK
        varchar tenant_id
        timestamptz created_at
    }

    TASKS {
        uuid id PK
        varchar tenant_id
        varchar workspace_id
        varchar title
        text description
        task_status status
        task_priority priority
        varchar creator_id
        varchar assignee_id "nullable"
        text_array labels
        jsonb metadata
        timestamptz due_at "nullable"
        timestamptz remind_at "nullable"
        timestamptz completed_at "nullable"
        timestamptz last_due_soon_notified_at "nullable"
        timestamptz last_overdue_notified_at "nullable"
        task_status status_before_blocked "nullable"
        varchar blocked_reason "nullable: dependency or manual"
        int version
        uuid parent_id FK "nullable, R2"
        timestamptz archived_at "nullable"
        int estimated_minutes "nullable, R2"
        jsonb agent_metadata "R2"
        timestamptz unblocked_at "nullable, R2"
        timestamptz created_at
        timestamptz updated_at
    }
    TASK_WATCHERS {
        uuid task_id PK, FK
        varchar user_id PK
        timestamptz created_at
    }
    TASK_COMMENTS {
        uuid id PK
        uuid task_id FK
        varchar author_id
        text body
        text_array mentions
        timestamptz created_at
    }
    NOTIFICATION_OUTBOX {
        uuid id PK
        varchar tenant_id
        uuid aggregate_id
        varchar event_type
        varchar idempotency_key UK
        jsonb payload
        jsonb trace_context
        outbox_status status
        int attempts
        int max_attempts
        timestamptz next_attempt_at
        timestamptz claimed_at "nullable"
        text last_error "nullable"
        timestamptz created_at
        timestamptz dispatched_at "nullable"
    }
    AGENT_JOBS {
        uuid id PK
        varchar tenant_id
        uuid task_id FK "nullable for user-scoped agents"
        varchar agent_type
        agent_job_status status
        varchar dedup_key UK
        int base_task_version "nullable"
        jsonb input_context
        jsonb proposed_actions "nullable"
        jsonb execution_result "nullable"
        text error_message "nullable"
        int attempts
        int conflict_count
        timestamptz retry_after "nullable"
        varchar decided_by "nullable"
        timestamptz decided_at "nullable"
        timestamptz created_at
        timestamptz updated_at
    }
    AGENT_RUN_HISTORY {
        uuid id PK
        uuid job_id FK
        int turn
        varchar model
        varchar tool_name "nullable"
        jsonb tool_args "nullable"
        jsonb tool_result "nullable"
        varchar outcome
        int prompt_tokens
        int completion_tokens
        int latency_ms
        timestamptz created_at
    }
    LEARNING_SUGGESTIONS {
        uuid id PK
        varchar tenant_id
        uuid job_id FK
        text event_type
        text label "nullable"
        int current_adjustment
        int proposed_adjustment
        int signal_count
        text rationale
        timestamptz created_at
    }
```

Not drawn: `workspace_rules`, which belongs to a workspace, and `webhook_deliveries`, `webhook_callbacks`, and `user_contacts`, which reference no other table. Section 4.2 describes them.

### 4.2 Table notes

| Table | Migration | Notes |
| --- | --- | --- |
| `tasks` | `000001`, extended by `000004` and `000006` | Full DDL for the base columns is in [initial.md](archive/initial.md) section 4.B. IDs are UUIDv7 generated in Go so they sort by creation time. `000006` adds `unblocked_at`: when the task last left `blocked` for `todo` or `in_progress`, by dependency release or by a person. The morning brief lists tasks unblocked in the 24 hours before it (D37). The task API does not expose it. |
| `task_watchers` | `000001` | Composite primary key `(task_id, user_id)`. |
| `task_comments` | `000001` | `mentions` is extracted from `@username` tokens at insert time. |
| `notification_outbox` | `000002` | One row per event per recipient. `idempotency_key` is unique. `aggregate_id` has no foreign key so outbox rows survive task deletion. |
| `task_dependencies` | `000004` | Replaces the `blocked_by UUID[]` column (decision 4). One row means `task_id` cannot start until `depends_on_task_id` is complete. Both foreign keys cascade on delete, which only fires on a permanent delete. A `CHECK (task_id <> depends_on_task_id)` blocks self-dependency. The foreign keys do not prove both tasks share a tenant, so the service checks that before insert. |
| `workspaces` | `000001`; `agent_settings` added in `000004` | Primary key `(tenant_id, id)`, with `name`. `tasks (tenant_id, workspace_id)` has a foreign key to it (decision 19). The table is created in `000001`, not `000004`, so the foreign key exists from the first task: adding it later would need a backfill of every workspace ID already used, and task creation would start failing for unknown workspaces. `agent_settings` defaults to agents off, approval required for everything, and a cap of 5 subtasks. The foreign key is `ON DELETE RESTRICT` (decision 24): a workspace cannot be deleted while any task references it, so an accidental delete cannot bypass soft archive, outbox scrubbing, or agent job cleanup. There is deliberately no workspace purge endpoint. If a workspace ever has to be decommissioned, it is done with an administrative script that verifies soft archives, drains the outbox, and cleans dependencies before touching the workspace row. |
| `agent_jobs` | `000004`, extended by `000005` | Base DDL in [agentic.md](archive/agentic.md) section 4.A, with the changes listed in section 7 below. `000005` adds `retry_after`, before which a pending job is not claimed (a failed run waits 30 s, 2 min, then 10 min; a version conflict 10 s), and `conflict_count`: the first 5 conflicts use no attempt (D24). A job's `updated_at` is its lease while it runs (D23). A morning brief is a digest job with no task, naming its person in `input_context.user_id` (D40); a learning job has no task either (D55). |
| `notification_acks` | migration `000010` (M14-06; `000001` to `000009` are taken) | `(tenant_id, user_id, notification_id)` primary key, `signal`, `acked_at`. Local read state for the in-app feed (section 9.6). Not shown in the diagram above; it references AgentPulse notification IDs, not Taskify rows. |
| `workspace_rules` | `000003` | `(tenant_id, workspace_id, rule_key)` primary key, `agentpulse_rule_id`. Records which default AgentPulse rules a workspace has, so `bootstrap-rules` can be repeated without creating duplicates. |
| `webhook_deliveries` | `000003` | Signature of each accepted AgentPulse callback, kept for an hour, so a replayed callback changes nothing. |
| `webhook_callbacks` | `000007` | `(tenant_id, callback_id)` primary key, `received_at`. Each applied callback's `callbackId`, kept for 7 days, so a retry of one click, signed again, changes nothing (AP-06). |
| `user_contacts` | `000007` | `(tenant_id, user_id)` primary key, nullable `email`, `slack_user_id`, `phone`, `updated_at`. The person's contact, forwarded to AgentPulse (AP-05). |
| `agent_run_history` | `000004` | One row per model turn. Named in the AG-102 backlog item but not defined in the source; this definition is proposed here. |
| `learning_suggestions` | `000008` | The learning suggestions Taskify submitted to AgentPulse (M11): tenant, learning job (foreign key, cascading), `event_type`, nullable `label`, current and proposed adjustments (each -20 to 20), `signal_count` (at least 5), `rationale`, `created_at`. Unique on job, event type, and label, so a job submits a pattern once; the learning agent leaves a pattern alone for 30 days after one was submitted (D57). |

Indexes:

| Index | Purpose |
| --- | --- |
| `idx_tasks_tenant_assignee_status (tenant_id, assignee_id, status)` | List and workload queries |
| `idx_tasks_due_scan (due_at, status)` partial on open tasks with a due date | Deadline scheduler |
| `task_dependencies` primary key `(task_id, depends_on_task_id)` | "What is this task waiting on?" |
| `idx_task_deps_reverse (tenant_id, depends_on_task_id)` | "Which tasks are waiting on this one?" |
| `idx_tasks_parent (parent_id)` partial where not null | Listing subtasks |
| `idx_outbox_poll (next_attempt_at, created_at)` partial where `status = 'pending'` | Relay claim |
| `idx_agent_jobs_poll (status, created_at)` partial where `status = 'pending'` | Agent runtime claim |
| `idx_agent_jobs_brief (tenant_id, input_context->>'user_id', created_at DESC)` partial where `agent_type = 'digest'` (`000006`) | A person's latest brief, `GET /me/brief` |
| `idx_webhook_callbacks_received (received_at)` (`000007`) | Deleting callback IDs older than 7 days |
| `idx_learning_suggestions_job_pattern` unique on `(job_id, event_type, COALESCE(label, ''))` (`000008`) | A job submits a pattern once |
| `idx_learning_suggestions_tenant (tenant_id, created_at)` (`000008`) | Patterns the tenant submitted within the 30-day window |

### 4.3 State machines

**Task status.** Transitions as decided (decision 9).

```mermaid
stateDiagram-v2
    [*] --> todo
    todo --> in_progress
    todo --> blocked
    todo --> completed
    in_progress --> blocked
    in_progress --> completed
    in_progress --> todo
    blocked --> in_progress
    blocked --> todo
    completed --> in_progress : reopen
    todo --> archived
    in_progress --> archived
    blocked --> archived
    completed --> archived
    archived --> todo : restore
```

| Current | Allowed targets |
| --- | --- |
| `todo` | `in_progress`, `completed`, `blocked`, `archived` |
| `in_progress` | `blocked`, `completed`, `todo`, `archived` |
| `blocked` | `in_progress`, `todo`, `archived` |
| `completed` | `in_progress` (reopen), `archived` |
| `archived` | `todo` (restore) |

Anything else fails with `ErrInvalidStateTransition` before any database or outbox write.

- Entering `blocked` emits `task.blocked`.
- Entering `completed` sets `completed_at` and emits `task.completed`. Reopening clears `completed_at`.
- Entering `archived` sets `archived_at`. Restoring clears it.
- List and scan queries exclude `archived`. A fetch by ID still returns an archived task, so it can be restored.

- `todo` → `completed` is allowed directly (decision 18), so "Mark done" works on a task that was never started. It emits one `task.completed` event and no synthetic `in_progress` step, which keeps cycle-time metrics honest.
- A `blocked` task cannot be completed directly; it must return to `in_progress` or `todo` first.
- Entering `blocked` records the status the task came from in `status_before_blocked` and why in `blocked_reason`. Both are cleared on leaving `blocked`. A check constraint keeps them in step with the status: `blocked_reason` is set exactly when `status = 'blocked'`.

**Two kinds of block (decision 23).**

| `blocked_reason` | Set when | Lifted by |
| --- | --- | --- |
| `manual` | A person moves the task to `blocked` through the API, CLI, frontend, or a notification action | A person only |
| `dependency` | The system blocks the task because it has an unresolved dependency | The system, when the last dependency resolves; or a person |

The system applies a dependency block in three situations, and only to tasks in `todo`:

1. The decomposer creates a child that depends on a sibling. The child is inserted as `blocked` with `status_before_blocked = 'todo'`.
2. `LinkDependencies`, or a person, adds an unresolved dependency to a `todo` task.
3. An archived blocker is restored and a dependent is still `todo`.

A task that is already `in_progress` is never pulled back to `blocked` by the system. It keeps its status and receives a system comment and a `task.dependency_added` or `task.dependency_reopened` notification, so the assignee decides. This follows the same reasoning as the approved restore behaviour: the system should not override the status of work someone has started.

System-applied dependency blocks are quiet. They carry actor kind `system`, so they do not start the blocker agent, and their `task.blocked` event is sent with `blockedReason: "dependency"` and without `bypassDigest`. Only manual blocks get the immediate routing: `bypassDigest`, a `priorityHint` of at least `high` (`critical` when the task itself is `p1_critical`), and the blocker agent. Without this, decomposing one task into five would fire several urgent alerts at once.

**Dependency cycles.** Before inserting a `task_dependencies` row, the service runs a recursive query from `depends_on_task_id` following its own dependencies; if it reaches `task_id`, the insert is rejected. The tasks involved are locked with `SELECT ... FOR UPDATE` in ID order, so two concurrent inserts cannot each pass the check and together form a cycle.

**Dependency resolution (decision 17).** A dependency is resolved when the task it points to is `completed` or `archived`. Archiving is a status change, not a row delete, so the `task_dependencies` row stays.

```sql
-- Does this task still have an open blocker?
SELECT EXISTS (
    SELECT 1
    FROM task_dependencies td
    JOIN tasks t ON td.depends_on_task_id = t.id
    WHERE td.task_id = $1
      AND td.tenant_id = $2
      AND t.status NOT IN ('completed', 'archived')
);
```

```mermaid
sequenceDiagram
    autonumber
    participant S as TaskService
    participant DB as PostgreSQL

    Note over S,DB: inside the transaction that completes or archives task A
    S->>DB: SELECT dependents of A from task_dependencies
    loop each dependent B
        S->>DB: does B still have an open blocker?
        alt no open blockers
            alt B is blocked with reason dependency
                S->>DB: UPDATE B to status_before_blocked, clear both columns
                S->>DB: INSERT outbox task.unblocked for B, naming A and whether it was completed or archived
            else B is blocked with reason manual
                S->>DB: INSERT system comment on B: dependency resolved, task remains blocked manually
                S->>DB: INSERT outbox task.dependency_resolved for B
            else B is not blocked
                S->>DB: INSERT outbox task.dependency_resolved for B
            end
        end
    end
```

- The check and the status change run in the same transaction as the change to A, with actor kind `system`, so they never enqueue agent jobs.
- A manually blocked task is never unblocked by the system. If someone blocked a task because of a vendor outage, finishing an unrelated subtask leaves it blocked, with a comment saying the dependency is done.
- A dependent that is `in_progress` (it was started despite the dependency) keeps its status and gets `task.dependency_resolved`.
- **Restore.** If A is restored, its dependency rows are still there, so it counts as an open blocker again. Dependents still in `todo` are given a dependency block. Dependents that are `in_progress` or `completed` keep their status and get a system comment plus a high-priority `task.dependency_reopened` notification, so the assignee can judge whether the revived blocker matters.

**Outbox row.**

```mermaid
stateDiagram-v2
    [*] --> pending
    pending --> processing : claimed by relay
    processing --> dispatched : 2xx
    processing --> pending : retryable failure, backoff
    processing --> pending : lease expired
    processing --> dead_letter : attempts exhausted or non-retryable 4xx
    dispatched --> [*] : deleted after 7 days
    dead_letter --> pending : manual replay
```

**Agent job.**

```mermaid
stateDiagram-v2
    [*] --> pending
    pending --> running : claimed
    running --> completed : only Tier 1 actions, all applied
    running --> awaiting_approval : Tier 2 proposal stored
    running --> pending : version conflict, retry with fresh context
    running --> failed : model or tool error, attempts exhausted
    awaiting_approval --> completed : approved and applied
    awaiting_approval --> dismissed : rejected
    awaiting_approval --> failed : stale_version at apply time
    completed --> [*]
    dismissed --> [*]
    failed --> [*]
```

### 4.4 Idempotency and dedup keys

| Case | Key format |
| --- | --- |
| State mutation | `taskify:{tenant_id}:{task_id}:{event_type}:v{version}` |
| Mutation with several recipients | Same, with `:{user_id}` appended |
| Due soon | `taskify:{tenant_id}:{task_id}:due_soon_24h:{due_at as Unix seconds}` and `...:due_soon_1h:{...}` (G2) |
| Overdue | `taskify:{tenant_id}:{task_id}:overdue:{YYYY-MM-DD}` |
| Comment | `taskify:{tenant_id}:{task_id}:task.comment_added:{comment_id}`, plus `:{user_id}` (D2) |
| Dependency notices (`task.dependency_added`, `_resolved`, `_reopened`) | `taskify:{tenant_id}:{task_id}:{event_type}:{blocker_id}:v{blocker_version}` (D17) |
| Approval request | `taskify:{tenant_id}:{task_id}:agent.approval_requested:{job_id}` |
| Escalation | `taskify:{tenant_id}:{task_id}:task.escalated:{job_id}:{user_id}`, always with the recipient (D30) |
| Morning brief | `taskify:{tenant_id}:{job_id}:digest.morning_brief` (D38) |
| Contact forward (one row per person, reused for every change; D46) | `taskify:{tenant_id}:contact.update:{user_id}` |
| Agent job dedup | `{agent_type}:{task_id}:v{version}` for event-triggered jobs; `decomposer:{task_id}:request:{uuid}` for each requested decomposition (D29); `{agent_type}:{task_id}:stale:{YYYY-MM-DD}` for stale scans; `digest:{tenant_id}:{user_id}:brief:{YYYY-MM-DD}` for morning briefs (D40); `learning:{tenant_id}:week:{YYYY-Www}` for the weekly learning job (D55) |
| Learning suggestion (Taskify's own; AgentPulse takes no key for suggestions) | `taskify:{tenant_id}:{job_id}:learning.suggestion:{hash of event type and label}` |

Changing `due_at` clears `last_due_soon_notified_at`, and the due-soon key includes the due time, so a rescheduled task is reminded again (G2). This is not covered in the source documents.

---

## 5. Low-level design

### 5.1 Package layout

```text
taskify/
├── contract/                        # R3: language-neutral definition, see 9.2
├── cmd/taskify/main.go              # wiring, graceful shutdown
├── cmd/devtoken/                    # prints a signed development JWT
├── cmd/mock-agentpulse/             # runs the mock AgentPulse for local use
├── cmd/taskctl/                     # R3: CLI, see 9.5
├── pkg/client/                      # R3: Go API client generated from the contract
├── sdk/python/                      # R3: Python client SDK, see 9.4
├── web/                             # R3: frontend, see 9.6; embed.go for optional embedding
├── tests/
│   ├── mockagentpulse/              # the mock AgentPulse: the agreed /v1/ contract, recorded and scriptable
│   ├── e2e/                         # end-to-end tests through the HTTP API
│   ├── bench/                       # benchmarks
│   └── conformance/                 # R3: black-box suite, see 9.3
├── internal/
│   ├── app/                         # wiring shared by the server and end-to-end tests
│   ├── config/config.go             # env parsing and validation
│   ├── domain/
│   │   ├── task.go                  # Task, state machine
│   │   ├── comment.go, dependency.go, subtask.go
│   │   ├── event.go                 # event types, recipients, payload and key builders
│   │   ├── tenant.go, user.go       # tenant and user ID rules (AP-04, AP-11)
│   │   ├── agentjob.go              # AgentJob, statuses, tiers (R2)
│   │   ├── approval.go, escalation.go, brief.go, learning.go   # agent events and reports (R2)
│   │   ├── contact.go               # a person's contact (AP-05)
│   │   └── errors.go
│   ├── repository/
│   │   ├── repository.go            # interfaces
│   │   ├── postgres/                # one file per repository
│   │   ├── memory/                  # in-memory fakes with the same interfaces
│   │   └── repotest/                # one conformance suite run against both
│   ├── service/
│   │   ├── task_service.go
│   │   ├── dependency_service.go, decompose.go, subtasks.go
│   │   ├── feedback_service.go
│   │   ├── workspace_service.go     # workspaces and default rules
│   │   ├── agent_job_service.go     # apply and dismiss (R2)
│   │   ├── contact_service.go       # PUT /me/contact (AP-05)
│   │   └── learning_service.go      # records and queues learning suggestions (M11)
│   ├── worker/
│   │   ├── outbox_relay.go
│   │   ├── deadline_scheduler.go    # reminders, stale scan, briefs, learning jobs
│   │   └── outbox_cleanup.go
│   ├── agentpulse/
│   │   ├── client.go                # typed calls
│   │   └── breaker.go               # circuit breaker
│   ├── agent/                       # R2
│   │   ├── runtime.go               # job claim loop, reasoning loop, optional agent hooks
│   │   ├── agent.go                 # Agent interface
│   │   ├── llm/                     # client.go, mock.go
│   │   ├── tools/                   # registry.go, schema.go, and one file per tool
│   │   ├── triage.go
│   │   ├── decomposer.go
│   │   ├── blocker.go
│   │   ├── digest.go
│   │   └── learning.go
│   ├── api/
│   │   ├── router.go
│   │   ├── middleware.go, auth.go
│   │   ├── task_handler.go
│   │   ├── webhook_handler.go
│   │   ├── admin_handler.go
│   │   ├── agent_job_handler.go     # R2, with GET /me/brief
│   │   └── contact_handler.go       # GET and PUT /me/contact
│   └── observability/               # metrics.go, tracing.go
├── migrations/
│   ├── 000001_init_tasks.{up,down}.sql
│   ├── 000002_outbox.{up,down}.sql
│   ├── 000003_integrations.{up,down}.sql
│   ├── 000004_agent_schema.{up,down}.sql
│   ├── 000005_agent_job_retry.{up,down}.sql
│   ├── 000006_digest.{up,down}.sql
│   ├── 000007_agentpulse_v1.{up,down}.sql
│   └── 000008_learning.{up,down}.sql
├── docker-compose.yml
└── Makefile
```

Dependency direction: `api` → `service` → `domain` and repository interfaces. `worker` and `agent` → `service`, repository interfaces, and adapters. `domain` imports nothing from the project.

### 5.2 Domain and service class diagram

```mermaid
classDiagram
    class Task {
        +UUID ID
        +string TenantID
        +string WorkspaceID
        +string Title
        +string Description
        +Status Status
        +Priority Priority
        +string CreatorID
        +string AssigneeID
        +[]string Labels
        +map Metadata
        +Time DueAt
        +int Version
        +UUID ParentID
        +[]UUID DependsOn
        +Time ArchivedAt
        +int EstimatedMinutes
        +Apply(patch, actor) []Event
        +TransitionTo(status, actor) []Event
        +CanTransition(to) bool
    }
    class Comment {
        +UUID ID
        +UUID TaskID
        +string AuthorID
        +string Body
        +[]string Mentions
        +ExtractMentions(body) []string
    }
    class Event {
        +string Type
        +string TenantID
        +UUID TaskID
        +string ActorID
        +ActorKind ActorKind
        +int Version
        +map Payload
        +IdempotencyKey(recipient) string
        +Recipients(task) []string
    }
    class OutboxEntry {
        +UUID ID
        +UUID AggregateID
        +string EventType
        +string IdempotencyKey
        +json Payload
        +json TraceContext
        +OutboxStatus Status
        +int Attempts
        +Time NextAttemptAt
    }
    class TaskRepository {
        <<interface>>
        +Create(tx, task) error
        +Get(ctx, tenantID, id) Task
        +GetForUpdate(tx, tenantID, id) Task
        +Update(tx, task, expectedVersion) error
        +List(ctx, filter, cursor, limit) []Task
        +ClaimDueSoon(tx, now, limit) []Task
        +ClaimOverdue(tx, now, limit) []Task
    }
    class OutboxRepository {
        <<interface>>
        +Insert(tx, entries) error
        +ClaimBatch(ctx, limit) []OutboxEntry
        +MarkDispatched(ctx, id) error
        +MarkRetry(ctx, id, nextAttempt, err) error
        +MarkDeadLetter(ctx, id, err) error
        +ReleaseExpiredLeases(ctx, olderThan) int
        +DeleteDispatchedBefore(ctx, cutoff, limit) int
    }
    class TxManager {
        <<interface>>
        +WithTx(ctx, fn) error
    }
    class TaskService {
        +CreateTask(ctx, cmd) Task
        +UpdateTask(ctx, cmd) Task
        +CompleteTask(ctx, cmd) Task
        +ArchiveTask(ctx, cmd) error
        +AddComment(ctx, cmd) Comment
        +GetTask(ctx, tenantID, id) Task
        +ListTasks(ctx, filter) Page
    }
    class Notifier {
        <<interface>>
        +Notify(ctx, req) error
        +RecordFeedback(ctx, fb) error
        +CreateRule(ctx, rule) error
        +ListLearningSuggestions(ctx, tenantID) []Suggestion
        +ReviewLearningSuggestion(ctx, id, decision) error
    }
    class OutboxRelay {
        -int workers
        -int batchSize
        -chan wake
        +Run(ctx) error
        +Wake()
    }
    class DeadlineScheduler {
        -Duration interval
        +Run(ctx) error
    }

    Task "1" o-- "*" Comment
    Task ..> Event : emits
    Event ..> OutboxEntry : serialised as
    TaskService --> TxManager
    TaskService --> TaskRepository
    TaskService --> OutboxRepository
    OutboxRelay --> OutboxRepository
    OutboxRelay --> Notifier
    DeadlineScheduler --> TaskRepository
    DeadlineScheduler --> OutboxRepository
```

### 5.3 Agent runtime class diagram

```mermaid
classDiagram
    class Agent {
        <<interface>>
        +Type() string
        +AllowedTools() []string
        +BuildContext(ctx, job) Messages
    }
    class Reporter {
        <<interface>>
        +Gather(ctx, job) Report
    }
    class Finisher {
        <<interface>>
        +FinishEvents(ctx, job, now) []Event
    }
    class TriageAgent
    class DecomposerAgent
    class BlockerRemediationAgent
    class DigestAgent
    class LearningAgent
    class Runtime {
        -map agents
        -int workers
        -int maxTurns
        +Run(ctx) error
        -execute(ctx, job) error
    }
    class LLMClient {
        <<interface>>
        +Complete(ctx, req) Response
    }
    class OpenAICompatClient {
        -string baseURL
        -string model
        -Duration timeout
        +Complete(ctx, req) Response
    }
    class MockLLM {
        +Script []Response
        +Complete(ctx, req) Response
    }
    class Tool {
        <<interface>>
        +Name() string
        +Description() string
        +Parameters() json
        +Tier(args) SafetyTier
        +Execute(ctx, scope, args) any
    }
    class ToolRegistry {
        -map tools
        +Register(tool)
        +Schemas(names) []ToolSchema
        +Dispatch(ctx, scope, call) Result
    }
    class Scope {
        +string TenantID
        +UUID TaskID
        +UUID JobID
        +int BaseVersion
    }
    class AgentJob {
        +UUID ID
        +string TenantID
        +UUID TaskID
        +string AgentType
        +JobStatus Status
        +json InputContext
        +json ProposedActions
        +json ExecutionResult
        +int BaseTaskVersion
    }
    class AgentJobService {
        +Enqueue(tx, job) error
        +Apply(ctx, jobID, decidedBy) error
        +Dismiss(ctx, jobID, decidedBy) error
    }

    Agent <|.. TriageAgent
    Agent <|.. DecomposerAgent
    Agent <|.. BlockerRemediationAgent
    Agent <|.. DigestAgent
    Agent <|.. LearningAgent
    Agent <|-- Reporter
    Reporter <|.. DigestAgent
    Reporter <|.. LearningAgent
    Finisher <|.. BlockerRemediationAgent
    LLMClient <|.. OpenAICompatClient
    LLMClient <|.. MockLLM
    Runtime --> Agent
    Runtime --> LLMClient
    Runtime --> ToolRegistry
    Runtime --> AgentJob
    ToolRegistry o-- Tool
    ToolRegistry ..> Scope
    AgentJobService --> AgentJob
```

A `Reporter` is an agent whose model writes a report from facts that Go code gathers (the morning brief, the learning review). The runtime calls `Gather` in place of `BuildContext`; a nil report or a `Skip` error completes the job as skipped without calling the model. The report's `Deliver` turns the model's text into the stored report and its notifications, which the runtime writes in the transaction that completes the job. A `Finisher` sends notifications of its own when a run ends well: `FinishEvents` runs in the transaction that saves the job, so its events exist only for a run that completed, and the model has no say in what is sent or to whom. The blocker uses it for `task.escalated`.

`Scope` is built from the job row, never from model output. A tool that receives a `task_id` argument outside the scope's tenant, or outside the job's task and its children, returns an error.

### 5.4 REST API

All routes are under `/api/v1`, except the operations routes. Legacy unprefixed `/tasks` routes alias the task endpoints. [contract/openapi.yaml](../contract/openapi.yaml) is the authority for every path, parameter, and response; this table is a summary. Rows marked "Not built" are planned and are not in the contract yet.

| Method and path | Purpose | Events or effects | Release |
| --- | --- | --- | --- |
| `POST /tasks` | Create task | `task.created`, `task.assigned` | R1 |
| `GET /tasks` | List, newest update first, with filters and cursor: `assignee_id`, `status`, `priority`, `id_prefix` (a prefix of the canonical ID, for short IDs), `limit`, `cursor` | none | R1; `id_prefix` M12 |
| `GET /tasks/{id}` | Task with watchers and recent comments | none | R1 |
| `PUT /tasks/{id}`, `PATCH /tasks/{id}` | Update; requires `If-Match` or `version` | `task.updated`, `task.assigned`, `task.priority_escalated`, `task.blocked` | R1 |
| `POST /tasks/{id}/complete` | Complete | `task.completed`; `acted` feedback if a notification ID is supplied | R1 |
| `DELETE /tasks/{id}` | Soft archive: sets `status = 'archived'`, `archived_at`, `updated_at` | none | R1 |
| `DELETE /tasks/{id}?permanent=true` | Permanent delete for data-scrubbing requests (admin only); answers `204` | Removes the task and its dependent rows | R1 |
| `POST /tasks/{id}/comments` | Add comment, extract mentions, detect `/decompose` | `task.comment_added` | R1 |
| `POST /tasks/{id}/feedback` | Forward `acted`, `dismissed`, `snoozed`, `muted` | AgentPulse `/feedback` | R1 |
| `POST /webhooks/agentpulse` | Signed callbacks from AgentPulse | Task mutation plus feedback | R1 |
| `POST /workspaces/{id}/bootstrap-rules` | Register default rules | AgentPulse `/rules` | R1 |
| `GET /admin/learning/suggestions` | List AgentPulse's learning suggestions (admin), in snake_case like the rest of the API (D59) | proxy | R1 |
| `POST /admin/learning/suggestions/{id}/decision` | Approve or reject (admin) | proxy | R1 |
| `POST /tasks/{id}/decompose` | Queue decomposition; returns `202` with job ID | `agent_jobs` row | R2 |
| `GET /agent-jobs/{id}` | Job status, proposal, result. Visible by kind: a job on a task to any member of the tenant, a morning brief only to its person and admins, a tenant-wide job (the learning run) to admins only; otherwise `404` (D61) | none | R2 |
| `POST /agent-jobs/{id}/apply` | Approve a Tier 2 proposal | Applies changes, emits events | R2 |
| `POST /agent-jobs/{id}/dismiss` | Reject a Tier 2 proposal | none | R2 |
| `GET /agent-jobs` | List the jobs the caller may see, newest first; filters `status`, `task_id`, `agent_type`, `limit`, `cursor`. A job the caller may not see is left out. Feeds the approvals inbox. | none | R3 (M12) |
| `GET /tasks/{id}/subtasks` | Children of a task with their dependency links | none | R3 (M12) |
| `POST /tasks/{id}/restore` | Restore an archived task to `todo` | none | R1 |
| `POST /workspaces` | Create a workspace (admin). Tasks can only be created in an existing workspace. | Registers the default AgentPulse rules | R1 |
| `GET /workspaces/{id}/settings`, `PATCH /workspaces/{id}/settings` | Read and change workspace settings, including `agent_settings` (admin) | none | R1; agent fields from R2 |
| `GET /tasks/{id}/agent-runs` | Agent activity for a task, oldest first, from `agent_jobs` and `agent_run_history` | none | R3 (M12) |
| `GET /admin/outbox` | List the tenant's outbox rows, newest first (admin); filter `status` (such as `dead_letter`), `limit`, `cursor`. Dispatched rows are kept 7 days. | none | R3 (M12) |
| `POST /admin/outbox/{id}/replay` | Return a dead-letter row to `pending` (admin) | Row is retried | R3 (M12) |
| `GET /notifications/in-app` | Current user's in-app feed, proxied from AgentPulse and enriched with task data. Not built. | none | R3 (M14-06) |
| `POST /notifications/{id}/ack` | Mark a notification read or dismissed. Not built. | Local ack plus AgentPulse feedback | R3 (M14-06) |
| `GET /me` | Current user, tenant, and role | none | R3 (M12) |
| `GET /me/brief` | The caller's newest morning brief; `404` when there is none | none | R2 (M10) |
| `GET /me/contact`, `PUT /me/contact` | The caller's own contact for AgentPulse: `email`, `slack_user_id`, `phone` (section 5.5) | `contact.update` outbox row | R2 (AP-05) |
| `GET /events` | Server-Sent Events stream | none | Later (live updates Phase 2) |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | Operations (not under `/api/v1`) | none | R1 |

Single-task reads and writes return an `ETag`. `updated_since` and `If-None-Match` on list endpoints are planned for the frontend's polling (M14) and are not built.

The R3 rows exist because the CLI and frontend need to list things that R1 and R2 only expose one at a time. M12 built the list endpoints and `GET /me`; the notification feed is M14-06.

**Permanent delete.** One transaction, in this order:

1. Take the tenant's brief lock exclusively (D48), then lock the task.
2. Delete `agent_jobs` rows for the task (their run history cascades).
3. Delete every `notification_outbox` row with that `aggregate_id`, in any status. The outbox has no foreign key to `tasks` and its `payload` holds the task title.
4. Delete the morning briefs that name the task (AP-08, D48): the digest jobs whose stored report lists it among its top items, pending approvals, or unblocked tasks, with their run history, and every outbox row of theirs, in any status.
5. Delete the task. Foreign keys cascade to `task_dependencies`, `task_watchers`, and `task_comments`.
6. If no task row was deleted, return `ErrNotFound` and roll back.

Notes:

- Removing `pending` and `processing` rows stops the relay from sending a notification about a task that no longer exists. A relay worker that claimed a row a moment before the delete may still send it once; its later status update then matches no row and must be treated as success, not an error.
- Dependents of the deleted task lose that dependency row through the cascade. The unblock check from section 4.3 runs for them after the task row is deleted, in the same transaction, as if the task had been resolved (D18).
- Briefs that name the task are deleted with it, sent outbox rows included, so no copy of the title stays in Taskify. A brief scan later the same day may write a second brief without the task (D48, D53).
- AgentPulse may still hold delivered notifications that contain the title. Taskify can write a `taskify.task_scrubbed` event after the delete, carrying only the task ID, so AgentPulse can mask its copy. This is optional and depends on AgentPulse having a way to act on it, which is not confirmed (M0).

Error mapping:

| Domain error | HTTP status |
| --- | --- |
| `ErrValidation`, `ErrInvalidStateTransition` | 400 or 422 |
| Missing or invalid credentials, bad webhook signature | 401 |
| `ErrNotFound` (including a task in another tenant) | 404 |
| `ErrVersionConflict` | 409 |
| Rate limit | 429 |

### 5.5 AgentPulse contract used by Taskify

This is the `/v1/` API that AgentPulse builds in its milestone 40 (slices 40a to 40d), as agreed with AgentPulse on 2026-11-22 ([blockers.md](blockers.md), "Agreed with AgentPulse"). Taskify's client is `internal/agentpulse`, and the mock in `tests/mockagentpulse` serves the same paths and enforces AgentPulse's validation; any change to the contract changes the mock in the same commit. Task M0-07 runs the client and webhook tests against a real AgentPulse once these endpoints ship.

**Transport.** Every call sends `Authorization: Bearer <AGENTPULSE_API_KEY>` (AgentPulse also accepts `X-API-Key`). JSON fields are camelCase. The health probe is `GET /healthz`.

**Tenant IDs** match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, are compared case-sensitively, are never trimmed, and `default` is refused. Taskify applies the same rule to every identity it accepts (the JWT `tenant_id` claim, the development headers, `cmd/devtoken`) and in domain validation, so no tenant can exist in Taskify that AgentPulse would refuse.

**User IDs** are 1 to 128 printable ASCII characters, space to tilde, since they appear in idempotency keys (AP-11). Taskify applies the rule to the JWT subject and the development headers (after trimming), `cmd/devtoken`, a task's creator, assignee, and watchers, a comment's author, a contact's owner, and a webhook callback's `userId`.

**Tenant scoping.** Every call that names an object by ID (a rule, a learning suggestion, a notification) carries `tenantId`; AgentPulse answers `400` without it and `404` for another tenant's object, as for an unknown ID (AP-12).

| Taskify call | AgentPulse endpoint | Body or query |
| --- | --- | --- |
| `Notify` (outbox relay) | `POST /v1/notify` | The fields below |
| `PutContact` (outbox relay) | `PUT /v1/tenants/{t}/users/{u}/contact` | `{email, slackUserId, phone}` |
| `RecordFeedback` | `POST /v1/feedback` | `{notificationId?, tenantId, userId, signal, snoozeDuration?, taskId?}`; `signal` is `acted`, `dismissed`, `snoozed`, or `muted`. At least one of `notificationId` and `taskId`; Taskify always sends the task and leaves `notificationId` out when it has none. Without a notification, AgentPulse counts the signal under the user's latest notification about the task (AP-12). A `notificationId` AgentPulse does not hold for the tenant answers `404`. |
| `CreateRule` | `POST /v1/rules` | `{tenantId, name, attentionClass?, matchAny: [{eventTypes?, payloadMatch?}], channels, bypassDigest?}`, answered with `201 {"id"}` |
| `ListRules` | `GET /v1/rules?tenantId=` | `{"rules": [...]}` |
| `ListLearningSuggestions` | `GET /v1/learning/suggestions?tenantId=` | `{"suggestions": [{id, status, suggestion}]}` |
| `SubmitLearningSuggestion` (M11, outbox relay) | `POST /v1/learning/suggestions` | `tenantId`, `ruleId` or `eventPattern` (`{eventType, label}`, `label` null for no label), `currentAdjustment`, `proposedAdjustment` (within ±20, AgentPulse's heuristic score shift for that scope), `signalCount` (at least 5 signals from at least 2 users), `rationale`. Stored as `PENDING_APPROVAL`; nothing changes until an admin decides. Taskify sends it from a `learning.suggestion` outbox row (D57). |
| `LearningSuggestions` (M11) | `GET /v1/learning/suggestions?tenantId=` | The same list, decoded, for the learning agent: pending suggestions and the latest approved adjustment per pattern |
| `ReviewLearningSuggestion` | `POST /v1/learning/suggestions/{id}/decision` | `{tenantId, decision: approve or reject, decidedBy}`; `404` for another tenant's suggestion (AP-12), `409` if already decided |
| `FeedbackAnalytics` (M11) | `GET /v1/analytics/feedback?tenantId=&since=` | `{since, groups: [{eventType, label, delivered, acted, dismissed, muted, snoozed, distinctUsers}]}`, one group per event type and payload label, `label` null for notifications without one (R7, G16) |
| `InAppFeed` (M14-06) | `GET /v1/tenants/{t}/users/{u}/in-app-feed?limit=&cursor=` | `{"notifications": [...], "nextCursor"}`, newest first; Taskify keeps read state |
| Inbound | `POST /api/v1/webhooks/agentpulse` | See "Callbacks" below |

`/v1/notify` fields:

| Field | Type | Notes |
| --- | --- | --- |
| `idempotencyKey` | string, required | Up to 320 printable ASCII characters, stored as sent, unique per tenant. A repeat answers `200 {"id", "duplicate": true}`. |
| `tenantId` | string, required | |
| `userId` | string, required | The recipient. AgentPulse keys budget and preferences on `(tenantId, userId)`. |
| `source` | string | `taskify` |
| `eventType` | string, required | |
| `title`, `body` | string, required | |
| `channels` | string array | `slack`, `email`, `in_app`, `webhook`; `digest` alone delivers only in the user's digest |
| `priorityHint` | string | `critical`, `high`, `medium`, `low`. Only an input to rules. Mapping: `p1_critical` → `critical`, `p2_high` → `high`, `p3_medium` → `medium`, `p4_low` → `low`. |
| `payload` | object | Free-form and matchable by rules, plus the reserved keys below |

Reserved `payload` keys:

| Key | Effect |
| --- | --- |
| `bypassDigest: true` | Keeps the item out of the digest. Budget and quiet hours still apply, and it never makes an item critical. Taskify sets it for `p1_critical` tasks, manual blocks, and `task.escalated`. |
| `digestSummary` | The digest's line for this item. A held assignment reads `Assigned to you: {title}. Deferred to avoid context switching`; a morning brief carries its text. |
| `actionUrls` | Action buttons, from the fixed set below |
| `actionLabels` | Optional button text per action key |

All payload keys are camelCase. `bypass_digest` is a legacy spelling: the serializer always writes `bypassDigest`, and the client rewrites a legacy `bypass_digest` before sending.

**Actions.** The `actionUrls` keys are fixed; from 40b an unknown key, or a key without the ID its callback needs, is refused with `400`. AgentPulse makes its own signed link for every key but `view`, which it uses as sent. A click opens a confirmation page, and the action runs when the button is pressed; each link works once and expires after 7 days. Without AgentPulse's `ACTIONS_PUBLIC_BASE_URL`, messages carry only the view link.

| `actionUrls` key | Callback `action` | ID read from the payload | Default button text | Taskify sends it on |
| --- | --- | --- | --- | --- |
| `complete` | `complete` | `taskId` | Mark done | Task notifications while the task is open (D42) |
| `snooze_2h` | `snooze`, with `duration: "2h"` | `taskId` | Snooze 2h | Task notifications while the task is open |
| `approve` | `approve` | `jobId` | Approve | `agent.approval_requested` |
| `reject` | `reject` | `jobId` | Reject | `agent.approval_requested` |
| `unblock` | `unblock` | `taskId` | Unblock and set In Progress | `task.escalated` while the task is blocked |
| `view` | none, a plain link | | View | With `APP_BASE_URL`, every notification about a task, to `{APP_BASE_URL}/tasks/{id}`, and `digest.morning_brief`, to `{APP_BASE_URL}/brief`. Without it, no notification: an API URL needs a bearer token that a click cannot carry (AP-10, D50) |

**Rules.** `matchAny` is native: a rule applies when any entry matches. A rule may set `attentionClass` (`critical`, `standard`, `background`); a critical rule needs a real condition, event types or payload values, in every entry. AgentPulse runs its own locked critical rules first, then the tenant's rules, then defaults to `standard`. Each workspace gets three default rules, scoped by `payload.workspaceId`: `critical-or-blocked` (critical, `bypassDigest`, matching `priority: p1_critical`, `status: blocked` with `blockedReason: manual`, or `eventTypes: ["task.escalated"]`), `assignments-and-mentions`, and `low-priority-digest`.

**Contacts.** A person sets their own contact with `PUT /api/v1/me/contact` (`email`, `slack_user_id`, `phone`; an absent field is kept, `null` clears it) and reads it with `GET /api/v1/me/contact`. Taskify stores the latest contact per tenant and user (`user_contacts`) and forwards it through the outbox to `PUT /v1/tenants/{t}/users/{u}/contact`, always with all three fields. AgentPulse treats the PUT as a full replace: a `null` or missing field is cleared, a contact with all three empty is deleted, and the same body again changes nothing. A user with no contact gets in-app and digest delivery only. The formats are AgentPulse's, applied identically by Taskify (AP-12): `email` a plain address of at most 254 characters with no display name, `slack_user_id` `^[UW][A-Z0-9]+$` of at most 32 characters, `phone` `^\+[1-9][0-9]{1,14}$`; AgentPulse refuses a PUT with one bad field with a `400` naming it, which the relay dead-letters. Since AgentPulse applies PUTs in arrival order and keeps no version, each person has one outbox row, reused for every change: a change rewrites its payload; the relay never has one person's contact in flight twice; and a send that finishes after a newer change arrived returns the row to pending instead of marking it dispatched, so the newer contact is sent next (D46).

**Callbacks.** When a person clicks an action, AgentPulse's delivery queue calls Taskify:

- `POST {taskifyBaseUrl}/api/v1/webhooks/agentpulse`, which must be https and on AgentPulse's allow-list.
- Headers `X-AgentPulse-Timestamp` (Unix seconds) and `X-AgentPulse-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<raw body>">`, keyed with `AGENTPULSE_WEBHOOK_SECRET`. Timestamps more than 5 minutes from now are refused.
- Body `{action, tenantId, userId, taskId (complete, snooze, unblock), jobId (approve, reject), notificationId, duration (snooze), callbackId}`.
- `callbackId` is `cb-` and 32 hex digits, the same on every retry of one click while the timestamp and signature change. A callback is a duplicate when its `(tenantId, callbackId)` was seen before; without a `callbackId`, a byte-identical replay is recognised by its signature (D10). Taskify answers `200 {"status": "applied"}` or `{"status": "duplicate"}`, and forwards the matching feedback signal once.

**Slack.** One Slack workspace per AgentPulse deployment for now; per-tenant workspaces come in a later AgentPulse milestone.

### 5.6 Event taxonomy

| Event | Raised when | Recipients | Agent triggered |
| --- | --- | --- | --- |
| `task.created` | Task created with an assignee other than the creator | Assignee | Triage |
| `task.assigned` | `assignee_id` changes | New assignee | Digest gatekeeper |
| `task.updated` | Title, description, or due date changes | Assignee, watchers | Triage, if human-authored |
| `task.priority_escalated` | Priority moves upward | Assignee, watchers | none |
| `task.blocked` | Status becomes `blocked`. Payload carries `blockedReason`. | Assignee, watchers | Blocker, for manual blocks only |
| `task.dependency_resolved` (R2, new) | A dependency resolved but the task was not auto-unblocked (manually blocked, or already started) | Assignee | none |
| `task.dependency_added` (R2, new) | An unresolved dependency was added to a task that is already `in_progress`, so its status was left alone | Assignee | none |
| `task.dependency_reopened` (R2, new) | A resolved dependency became open again (blocker restored or reopened) for a task already started or completed. Sent with `priorityHint: high`. | Assignee | none |
| `task.due_soon` | Scheduler: 24 h and 1 h before `due_at` | Assignee | none |
| `task.overdue` | Scheduler: past `due_at`, not completed or archived | Assignee | none |
| `task.completed` | Status becomes `completed` | Creator, watchers | Dependency release |
| `task.comment_added` | Comment added | Assignee, watchers, mentioned users with `isMentioned: true` | Decomposer, on `/decompose` |
| `task.unblocked` (R2, new) | A dependency-blocked task was returned to its earlier status because all its dependencies are completed or archived | Assignee | none |
| `agent.approval_requested` (R2, new) | Agent job enters `awaiting_approval` | Assignee, or creator when unassigned | none |
| `task.escalated` (R2, new) | The blocker agent finished investigating a stale blocked task and found open blocking tasks. One event per distinct owner of those tasks, naming that owner's blockers in `payload.blockingTasks`. With no open blocker, the agent only drafts a comment and sends nothing (D30, D31). | Owner (assignee, or creator when unassigned) of each open blocking task, never the investigated task's people | none |
| `digest.morning_brief` (R2, new) | The digest agent wrote a person's morning brief (one per person per day, 08:00 UTC by default) | The person briefed | none |

The triage agent runs on every created task, including those where creator and assignee are the same and no notification is sent. The agent trigger is the task change, not the outbox row.

The actor is excluded from recipients: nobody is notified of their own action.

**Loop prevention.** Every event carries `ActorKind` (`user`, `agent`, `system`). Agent-authored changes still produce notifications but never enqueue agent jobs.

### 5.7 Tools and safety tiers

| Tool | Used by | Arguments | Tier |
| --- | --- | --- | --- |
| `UpdateTaskAttributes` | Triage | `task_id`, `labels`, `description`, `estimated_minutes`, `priority`, `reasoning` | Tier 1, except Tier 2 when `priority` is set |
| `LinkDependencies` | Triage, Blocker | `task_id`, `depends_on_task_ids` | Tier 1 |
| `CreateSubtaskDAG` | Decomposer | `parent_task_id`, `subtasks[]` with `title`, `description`, `depends_on` | Tier 1 (explicitly requested by a user) |
| `DraftResolutionComment` | Blocker | `task_id`, `body`, `reasoning` | Tier 1 |
| `ReassignTask` | Blocker | `task_id`, `assignee_id`, `reasoning` | Tier 2 |
| `ShiftDueDate` | Blocker | `task_id`, `due_at`, `reasoning` | Tier 2 |
| `ArchiveTask` | none by default | `task_id`, `reasoning` | Tier 2 |
| `ProposeNotificationRule` | Learning | `pattern_id`, `proposed_adjustment` (-20 to 20), `rationale`; the scope and evidence come from Go code | Tier 2, decided by an admin at AgentPulse: the call itself runs at Tier 1, even under `require_hitl_for_all`, and only submits the suggestion, which AgentPulse holds as `PENDING_APPROVAL` until an admin decides through `/admin/learning/suggestions` (D54) |

The digest agent is offered no tools: Go code gathers the facts and the model only writes the brief's text (D38). `ProposeNotificationRule` is offered to the learning agent, which also gets no other tool.

[agentic.md](archive/agentic.md) is inconsistent on due dates: section 1 requires approval only for shifts over 24 hours, section 4.C for any postponement. This document takes the stricter reading.

**Workspace agent controls (decision 10).** `workspaces.agent_settings`:

```json
{
  "agents_enabled": true,
  "disabled_agents": ["decomposer"],
  "require_hitl_for_all": true,
  "max_subtasks_per_decomposition": 5
}
```

| Setting | Where it is checked | Effect |
| --- | --- | --- |
| `agents_enabled` | At job enqueue, inside the task transaction | When false, no `agent_jobs` row is written. A skipped-job counter is incremented. |
| `disabled_agents` | At job enqueue | Jobs for the listed agent types are not written. An explicit `POST /tasks/{id}/decompose` for a disabled agent returns an error instead of `202`. |
| `require_hitl_for_all` | In the tool registry | Every tool call is treated as Tier 2, including ones a user requested explicitly, except `ProposeNotificationRule`, whose call only submits a suggestion that already waits for an admin (D54). |
| `max_subtasks_per_decomposition` | In `CreateSubtaskDAG` | A larger proposal is rejected and the model is asked once to reduce it. |

Defaults for a new workspace (decision 19): `agents_enabled: false`, `disabled_agents: []`, `require_hitl_for_all: true`, `max_subtasks_per_decomposition: 5`. Agents are opt-in so that no tenant incurs model costs or agent notifications without an admin choosing to. Note the second default: after an admin turns agents on, every agent action, including adding a label, still waits for approval until `require_hitl_for_all` is also turned off. The runtime checks the settings again when it claims a job, so a job queued before agents were switched off does not run.

Every Tier 1 execution appends to `tasks.agent_metadata`:

```json
{
  "runs": [
    {
      "job_id": "…",
      "agent": "triage",
      "at": "2026-11-11T09:30:00Z",
      "changes": {"labels": ["bug", "backend"], "estimated_minutes": 90},
      "reasoning": "Title mentions a crash in checkout on staging."
    }
  ],
  "priority_recommendation": {"value": "p1_critical", "job_id": "…"}
}
```

Reasoning loop limits: 30-second timeout per model call; retry with exponential backoff on 429 and 503; one repair turn for malformed output; a fixed maximum number of turns per job; a fixed maximum number of subtasks per decomposition.

### 5.8 Configuration

Read once at start-up by `internal/config`, which refuses to start on a missing required value or one out of range. Rows marked "Not built" are planned for the frontend and are not read yet.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port |
| `DATABASE_URL` | required | PostgreSQL connection string |
| `PUBLIC_BASE_URL` | required | Base for the API links in `actionUrls` in notifications |
| `APP_BASE_URL` | none | The web app's address. When set, every notification about a task carries `actionUrls.view` `{APP_BASE_URL}/tasks/{id}`, and the morning brief `{APP_BASE_URL}/brief`; unset, no notification carries a view link (AP-10, D50) |
| `AGENTPULSE_BASE_URL` | required | AgentPulse API |
| `AGENTPULSE_API_KEY` | required | Outbound authentication |
| `AGENTPULSE_WEBHOOK_SECRET` | required | HMAC key for inbound webhooks |
| `OUTBOX_WORKERS` | `8` | Relay concurrency |
| `OUTBOX_BATCH_SIZE` | `100` | Rows per claim |
| `OUTBOX_POLL_INTERVAL` | `200ms` | Relay ticker |
| `OUTBOX_MAX_ATTEMPTS` | `10` | Before dead-letter |
| `OUTBOX_LEASE` | `60s` | How long a claimed row may sit in `processing` before the lease reaper returns it to `pending` (D5) |
| `OUTBOX_RETRY_MAX_DELAY` | `60s` | Ceiling of the full-jitter backoff between sends of a row; lowered for a conformance target (M12-06) |
| `SCHEDULER_INTERVAL` | `1m` | Deadline scan period. The scan also queues the morning briefs and learning jobs. |
| `ENV` | `production` | `development` enables the header identity fallback |
| `JWT_ALGORITHM` | `HS256` | `HS256` or `RS256` |
| `JWT_SECRET` or `JWT_PUBLIC_KEY` | required in production (`JWT_SECRET` for HS256, `JWT_PUBLIC_KEY` for RS256) | Token verification key |
| `TRUSTED_GATEWAY_CIDRS` | empty | IP ranges allowed to send `X-Tenant-ID` and `X-User-ID` |
| `RATE_LIMIT_RPS` | `20` | Requests per second per tenant and user, in memory per replica |
| `RATE_LIMIT_BURST` | `40` | Token bucket size |
| `CORS_ALLOWED_ORIGINS` | empty | Not built. Frontend origins, when the frontend is deployed separately (M14) |
| `SERVE_EMBEDDED_SPA` | `false` | Not built. Serve the embedded frontend at `/` (M14) |
| `AGENTS_ENABLED` | `false` | Process-level switch for the agent runtime. Per-workspace control is in `agent_settings`. |
| `AGENT_WORKERS` | `4` | Agent job concurrency |
| `AGENT_MAX_TURNS` | `6` | Model turns per run |
| `AGENT_MAX_ATTEMPTS` | `3` | Failed runs before a job fails |
| `DIGEST_LOAD_THRESHOLD` | `3` | How many in-progress critical tasks make an assignee overloaded; a non-critical assignment to them then waits for their digest (D36) |
| `DIGEST_BRIEF_HOUR_UTC` | `8` | Hour of day, UTC, from which morning briefs are queued; no per-user time zone (D40) |
| `LLM_BASE_URL` | none | OpenAI-compatible endpoint. Required when `AGENTS_ENABLED=true`. |
| `LLM_API_KEY` | none | Empty for local Ollama |
| `LLM_MODEL` | none | Required when `AGENTS_ENABLED=true`. Model name. Development and CI: `qwen2.5-coder:7b` or `llama3.1:8b` on Ollama. Self-hosted production: a larger Qwen coder model on vLLM or TGI. Managed: any hosted model with tool calling. Switching is configuration only. |
| `LLM_TIMEOUT` | `30s` | Per-call timeout |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | none | Standard OpenTelemetry variables, read by the tracer and not by `internal/config`. Spans are exported over OTLP/HTTP only when one is set; the other `OTEL_*` settings follow the standard (D13). |

---

## 6. Cross-cutting concerns

### 6.1 Reliability

| Failure | Behaviour |
| --- | --- |
| AgentPulse down | Task writes succeed. Outbox rows accumulate and retry. The breaker opens for 10 seconds after more than 50% 5xx in a rolling 20-request window. |
| Relay crashes after AgentPulse accepted, before marking `dispatched` | Row is retried with the same idempotency key; AgentPulse deduplicates. |
| Relay crashes after claim | Lease reaper returns the row to `pending`. |
| Two replicas scan deadlines together | `SKIP LOCKED` plus unique `idempotency_key`. |
| Concurrent edits to one task | Second writer gets `409`. |
| LLM down or slow | Agent jobs retry, then fail. Task CRUD and notifications are unaffected. |
| LLM returns malformed output | One repair turn, then `failed`. No data changed. |
| Human edits a task while an agent is reasoning | Tool write fails on version; job re-runs with fresh context. |
| Human edits a task while a proposal awaits approval | Apply fails with `stale_version`. |
| Process shutdown | Stop accepting HTTP, cancel worker contexts, finish in-flight rows, close pool. |

Backoff: `next_attempt_at = now + random(0, min(60s, 500ms × 2^attempts))`. Retention: `dispatched` rows older than 7 days are deleted daily in batches of 5,000.

### 6.2 Performance targets

| Path | Target | How |
| --- | --- | --- |
| Task write | P95 under 15 ms, P99 under 30 ms | One local transaction, no remote calls |
| Task read | P95 under 8 ms | Composite index, keyset pagination on `(updated_at, id)` |
| Outbox dispatch | P95 under 150 ms, P99 under 400 ms | In-process wake-up plus 200 ms ticker; HTTP transport with `MaxIdleConnsPerHost: 64`, `IdleConnTimeout: 90s` |

These are targets from [initial.md](archive/initial.md). Nothing has been measured.

The in-process wake-up only reaches the relay in the same replica. Other replicas pick the row up on their next 200 ms tick. Postgres `LISTEN/NOTIFY` is the option if cross-replica latency matters.

### 6.3 Observability

Metrics:

| Metric | Type | Labels |
| --- | --- | --- |
| `taskify_http_request_duration_seconds` | histogram | route, method, status |
| `taskify_outbox_pending_rows` | gauge | none |
| `taskify_outbox_dispatch_duration_seconds` | histogram | event_type, outcome |
| `taskify_outbox_dlq_total` | counter | event_type |
| `taskify_feedback_forwarded_total` | counter | signal |
| `taskify_agent_job_duration_seconds` | histogram | agent_type, outcome |
| `taskify_agent_llm_tokens_total` | counter | agent_type, direction |
| `taskify_agent_proposals_total` | counter | agent_type, decision |
| `taskify_agent_jobs_skipped_total` | counter | agent_type, reason |

All are built, in `internal/observability`. `taskify_agent_jobs_skipped_total` counts jobs a task change asked for that were not queued, for example because agents are off for the workspace or the agent is disabled (D19).

Tracing: the handler stores the W3C `traceparent` in `notification_outbox.trace_context`. The relay restores it before calling AgentPulse, so one trace covers client, API, outbox, relay, and AgentPulse. Agent jobs carry the originating trace in `input_context`.

Logging: structured, with `request_id`, `tenant_id`, `task_id`, `job_id`, `trace_id`.

### 6.4 Security

- **Caller authentication (decision 2).**
  - Production: `Authorization: Bearer <token>`. Middleware verifies the JWT signature (HS256 or RS256) and reads `tenant_id` and `sub` (the user ID) into the request context under typed keys.
  - Development or gateway: when `ENV=development`, or the request comes from an address in `TRUSTED_GATEWAY_CIDRS`, `X-Tenant-ID` and `X-User-ID` are accepted without a token. With `ENV=production` and an untrusted source address, these headers are rejected.
  - The source address check must use the connection's peer address. `X-Forwarded-For` is caller-controlled and must not be trusted for this.
  - The webhook endpoint is the one exception: it authenticates by HMAC signature, not by JWT.
- **Approval ingress (decision 3).** Clicks in Slack or email reach Taskify only through the signed AgentPulse webhook. `POST /agent-jobs/{id}/apply` and `/dismiss` require a user JWT and are for the frontend and CLI.
- **Tenant isolation.** Repositories take `tenant_id` from the request context, never from the request body, and every query filters on it. A task in another tenant returns `404`. A tenant ID must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` exactly as given (never trimmed, case-sensitive) and must not be `default`, at every entry point: the JWT claim, the development headers, `cmd/devtoken`, and a webhook's `tenantId` (AP-04, D44).
- **Webhook authenticity.** `X-AgentPulse-Signature` is HMAC-SHA256 over timestamp and body, compared in constant time, with a 5-minute replay window. A retried click is recognised by its `(tenantId, callbackId)`, kept for 7 days; a callback without one, by its signature (AP-06, D47).
- **Prompt injection.** Task titles, descriptions, and comments are user-controlled text that the model reads. The controls are structural, not prompt-based:
  - each agent sees only its own allowed tools;
  - tenant and task scope come from the job row, not from model arguments;
  - arguments are validated against JSON Schema;
  - high-impact actions require human approval;
  - turn and subtask counts are capped.
- **Secrets.** API keys and the webhook secret come from the environment and are never logged. `.env` is already git-ignored.
- **Data sent to the LLM.** Task content leaves Taskify when a hosted model is configured. A local model (Ollama or vLLM) keeps it in-house. This is a deployment decision per tenant.
- **Auditability.** `agent_run_history` records every model turn and tool call; `agent_jobs` records who approved or rejected each proposal.

### 6.5 Concurrency model

| Goroutine group | Count | Coordination |
| --- | --- | --- |
| HTTP server | per request | none |
| Outbox relay | `OUTBOX_WORKERS` | `SKIP LOCKED` on `notification_outbox` |
| Deadline scheduler | 1 per replica | `SKIP LOCKED` on `tasks` |
| Agent runtime | `AGENT_WORKERS` | `SKIP LOCKED` on `agent_jobs` |
| Outbox cleanup and lease reaper | 1 per replica | idempotent statements |

All workers take a `context.Context` cancelled on shutdown and are joined with an `errgroup` in `main`.

---

## 7. Where this document departs from the source documents

| Topic | Source | This document | Reason |
| --- | --- | --- | --- |
| Parent column | `parent_task_id` in prose, `parent_id` in SQL | `parent_id` | The SQL is the more specific definition |
| Dependencies | `blocked_by UUID[]` with a GIN index | `task_dependencies` join table | Decision 4: referential integrity, cheap reverse lookups, cycle checks in SQL |
| `archived_at` | not present | New column on `tasks` | Decision 8 |
| `workspaces` table | `workspace_id` is only a string on `tasks` | New table with a foreign key from `tasks`, holding `agent_settings` | Decisions 10 and 19 |
| `status_before_blocked` | not present | New nullable column on `tasks` | Decision 17 returns an unblocked task to "`in_progress` if previously started", which needs the earlier status recorded |
| `blocked_reason` | not present | New nullable column on `tasks`: `dependency` or `manual` | Decision 23 |
| More new events | not present | `task.dependency_resolved`, `task.dependency_added`, `task.dependency_reopened` | Decisions 17 and 23 |
| `todo` → `completed` | not specified | Allowed | Decision 18 |
| Restore | `archived` not described as reversible | `archived` → `todo`, with `POST /tasks/{id}/restore` | Decision 9 |
| Overdue column | `last_notified_overdue_at` and `last_overdue_notified_at` both appear | `last_overdue_notified_at` | Matches the DDL |
| Admin routes | `/admin/notification-suggestions` and `/admin/learning/suggestions` both appear | `/admin/learning/suggestions` | Matches the implementation prompt |
| `agent_jobs.status` | `pending` means both "queued" and "waiting for approval" | Adds `awaiting_approval` and `dismissed` | One status cannot serve the worker poll index and the approval gate; a worker would re-claim jobs waiting on a human |
| `agent_jobs.task_id` | `NOT NULL` | Nullable | Digest and learning agents are scoped to a user or tenant, not a task |
| `agent_jobs` extra columns | not present | `dedup_key`, `base_task_version`, `attempts`, `decided_by`, `decided_at` | Duplicate suppression, stale-proposal detection, retry accounting, audit |
| `agent_run_history` | Named in AG-102, no definition | Defined in section 4.1 | Needed for audit |
| `project_id` | Listed in the domain model, absent from the DDL | Omitted | Follows the DDL; add when projects exist |
| Task IDs | UUIDv7 in prose, `gen_random_uuid()` (v4) in DDL | UUIDv7 generated in Go; DDL default kept as fallback | Time-ordered keys index better |
| New events | not present | `task.unblocked`, `agent.approval_requested` | Required by the decomposer and HITL flows |
| Agent triggering | "via the Outbox or an internal event bus" | `agent_jobs` rows written in the task transaction | Same durability guarantee as the outbox with no extra infrastructure |
| HITL order | AG-106 is P1, after triage | Before triage | Triage proposes priority changes, which need approval |
| Due-date shifts | Approval for shifts over 24 h in one place, for any postponement in another | Any shift needs approval | Stricter reading |

---

## 8. Decisions

### 8.1 Decided

Decisions 1 to 26 were made by the project owner on 2026-11-11, and decisions 27 to 40 by the owner on 2026-11-22 and 2026-11-23. Each is applied in the section named; the departures (D) are in [implementation/README.md](implementation/README.md) section 8. Numbers are kept from when the questions were first raised.

| # | Question | Decision | Applied in |
| --- | --- | --- | --- |
| 1 | Does the AgentPulse contract match? | Field names match and are camelCase. `priorityHint` values are `critical`, `high`, `medium`, `low`. The recipient field is `userId`. | 5.5 |
| 2 | How do callers authenticate? | JWT bearer in production with `tenant_id` and `sub` claims. `X-Tenant-ID` and `X-User-ID` headers only in development or from a trusted gateway address range. | 6.4, 5.8 |
| 3 | Where do approvals arrive? | Slack and other channel clicks through the signed AgentPulse webhook. Direct `apply` and `dismiss` endpoints require a user JWT, for frontend and CLI. | 6.4, 3.7 |
| 4 | Array or join table for dependencies? | `task_dependencies` join table, from the start. | 4.1, 4.2, 4.3 |
| 5 | Can Taskify force digest routing and supply digest text? | Yes: `channels: ["digest"]` forces digest, `payload.bypassDigest` (camelCase) forces immediate, `payload.digestSummary` supplies the text. | 5.5 |
| 6 | Does AgentPulse accept external learning suggestions? | Yes, through `POST /learning/suggestions`. They stay `PENDING_APPROVAL` until an admin decides. | 5.5 |
| 7 | Which model and hosting? | Ollama locally for development and CI. Production behind the OpenAI-compatible client, pointed at self-hosted vLLM or TGI where data must stay in-house, or a managed model otherwise. | 5.8 |
| 8 | Soft or hard delete? | Soft archive by default, setting `archived_at`. Permanent delete only through an admin-restricted `?permanent=true`. | 5.4 |
| 9 | Status transitions? | The matrix in section 4.3, enforced in the domain layer with `ErrInvalidStateTransition`. Amended by decision 18. | 4.3 |
| 10 | Per-workspace agent control? | `agent_settings` JSON on the workspace: master switch, per-agent disable list, force approval for everything, subtask cap. | 5.7, 4.2 |
| 11 | Python server or Python SDK? | SDK only. One Go server; the Python SDK is generated from `openapi.yaml` with a thin client on top. No second backend. | 9.1, 9.4 |
| 12 | How is parity kept across servers? | Not needed now. If a second server is ever built: Go is the reference, a black-box fixture-driven suite runs against both, and CI blocks merges unless both pass. | 9.3 |
| 13 | Where does the frontend read in-app notifications? | Through a Taskify proxy, `GET /api/v1/notifications/in-app`, with `POST /api/v1/notifications/{id}/ack`. The browser never calls AgentPulse. | 9.6 |
| 14 | How do live updates arrive? | Phase 1: polling with `ETag` and `304`. Phase 2: Server-Sent Events on `GET /api/v1/events`. No WebSockets. | 9.6 |
| 15 | How is the frontend deployed? | Separately on a CDN or static host in production. Optionally embedded in the Go binary with `go:embed`, switched on by `SERVE_EMBEDDED_SPA=true`, for local and self-hosted use. | 9.6 |
| 16 | CLI language? | Go, with `spf13/cobra` and `spf13/viper`. | 9.5 |
| 17 | Is a task still blocked when its blocker is archived? | No. A dependency is resolved when its target is `completed` or `archived`. The dependency row is kept. Dependents are re-checked, moved out of `blocked`, and sent `task.unblocked`. On restore the dependency is live again. | 4.3 |
| 18 | "Mark done" on a task that was never started? | Allowed: `todo` → `completed` is added to the matrix. No synthetic step through `in_progress`. | 4.3 |
| 19 | Workspace entity and agent defaults? | `workspaces` table with a foreign key from `tasks`. Agents default to off; approval for everything defaults to on. Endpoints `GET` and `PATCH /workspaces/{id}/settings`. | 4.2, 5.4, 5.7 |
| 20 | How does the event stream authenticate? | Streaming `fetch` with the normal bearer header. Never a JWT in the URL. A single-use 30-second ticket exchange is the fallback if a client needs native `EventSource`. | 9.6 |
| 21 | Where is the Python SDK published? | For M15, installed from a Git tag and subdirectory (`sdk/python`). No public wheels until v1.0. The public package name is reserved now. | 9.4 |
| 22 | CLI binary name? | `taskctl` for the CLI, `taskify` for the server. | 9.5 |
| 23 | Can auto-unblock override a manual block? | No. A `blocked_reason` column (`dependency` or `manual`) is added; only dependency blocks are lifted automatically. | 4.3 |
| 24 | `CASCADE` or `RESTRICT` on the workspace foreign key? | `ON DELETE RESTRICT`. | 4.2 |
| 26 | Python SDK package name? | Distribution name `taskify-sdk`, imported as `taskify`. Reserving the name on the public index is an owner action. | 9.4 |
| 25 | Permanent delete and the outbox? | One transaction deletes agent jobs, all outbox rows for the task, then the task. Optional `taskify.task_scrubbed` event for AgentPulse. | 5.4 |
| 27 | How does AgentPulse meet Taskify's contract? | Option B: AgentPulse is extended to Taskify's design, as its milestone 40. Option C was rejected. | 5.5; O1 |
| 28 | Paths, authentication, and identifiers | `/v1/` paths, Bearer authentication, `/healthz` for the health probe. Tenant IDs match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` and `default` is refused. User IDs are 1 to 128 printable ASCII characters. | 5.5; D44, D51 |
| 29 | How does AgentPulse get a person's contact? | Self-service `PUT /api/v1/me/contact`, forwarded through the outbox, in AgentPulse's formats. | 5.4, 5.5; D45, D46, D52 |
| 30 | Which events are critical by default? | The default rules make `p1_critical`, manual blocks, and `task.escalated` `attentionClass: critical`. | 5.5; D49 |
| 31 | Slack workspaces | One Slack workspace per AgentPulse deployment for now. | 5.5 |
| 32 | Action buttons and callbacks | Fixed action keys: `complete`, `snooze_2h`, `approve`, `reject`, `unblock`, `view`. `acknowledge` becomes `complete`. Callbacks are deduplicated on `(tenantId, callbackId)`. | 5.5; D42, D47 |
| 33 | The view link | `{APP_BASE_URL}/tasks/{id}`. While `APP_BASE_URL` is unset there are no links at all, brief items included. | 5.5, 5.8; D50 |
| 34 | Blocker escalation | A new event, `task.escalated`, to the owner of each open blocking task. With no open blocker, only a brief comment is written. | 5.6; D30, D31 |
| 35 | Digest gatekeeper and morning brief | G14 and G15 confirmed. A held assignment's digest line names the task. | 5.5, 5.8; D36 to D41 |
| 36 | Permanent delete and briefs | A permanent delete also deletes the briefs that name the task, sent outbox rows included. A same-day re-brief is accepted. | 5.4; D48, D53 |
| 37 | Learning suggestions | They skip `require_hitl_for_all`: the admin decides once, at AgentPulse. A pattern needs at least 50% of its notifications dismissed or muted, and 5 signals from 2 users in 30 days. | 5.7; D54, D56 |
| 38 | Identity provider (O2) | Deferred. Development tokens are used until one is chosen. | 9.5, 9.6; O2 |
| 39 | Who sees an agent job | By kind: a job on a task, any member of the tenant; a brief, only its person and admins; a tenant-wide job, admins only; otherwise `404`. | 5.4; D61 |
| 40 | Casing of the admin learning endpoints | `snake_case`, like the rest of the API. | 5.4; D58, D59 |

### 8.2 Open

No architecture question is open. What remains is waiting on others:

| Item | State |
| --- | --- |
| AgentPulse contract | Read against the real AgentPulse on 2026-11-22 and agreed (decision 27). Task M0-07 runs the client and webhook tests against a real AgentPulse once AgentPulse slices 40a to 40d ship (O1). |
| Identity provider (O2) | Deferred (decision 38). Blocks `taskctl login` (M13-02) and frontend sign-in (M14-03) only. |
| Public Python package name (O3) | Not needed until v1.0. |

---

## 9. Clients and portability

This section covers release R3: the contract, the Python client SDK, the CLI, and the web frontend. It follows decisions 11 to 16 and 20 to 22 in section 8.1.

### 9.1 Overview

There is one server, written in Go. Everything else is a client of its REST API (decision 11).

```mermaid
flowchart TB
    subgraph contractDir["contract folder"]
        oas["openapi.yaml"]
        ev["Event and tool schemas"]
        fx["Fixtures"]
    end
    subgraph clients["Clients"]
        cli["taskctl CLI, Go"]
        web["Web frontend, TypeScript"]
        pysdk["Python SDK"]
        other["Future SDKs in other languages"]
    end
    goSrv["Taskify Go server: API, outbox, scheduler, agents"]
    conf["Conformance suite"]
    pg[("PostgreSQL")]

    oas -->|"generate client"| cli
    oas -->|"generate types"| web
    oas -->|"generate models"| pysdk
    oas -.->|"generate"| other
    fx --> conf
    conf -->|"verifies over HTTP"| goSrv
    cli -->|"REST /api/v1"| goSrv
    web -->|"REST /api/v1"| goSrv
    pysdk -->|"REST /api/v1"| goSrv
    goSrv --> pg
```

"Other languages going forward" therefore means more generated SDKs, each a few days of work, not more servers.

### 9.2 Language-neutral contract

```text
contract/
├── openapi.yaml             # every endpoint, schema, and error body
├── events/                  # one JSON Schema per event payload
├── tools/                   # agent tool parameter schemas
├── fixtures/                # declarative request and expectation files
└── RULES.md                 # rules a schema cannot express
tests/
└── conformance/             # runner that executes the fixtures against TARGET_URL
```

| Artifact | Normative for | Consumed by |
| --- | --- | --- |
| `openapi.yaml` | Paths, methods, request and response bodies, status codes | Go response validation in CI, `pkg/client`, frontend types, Python SDK models |
| `events/*.schema.json` | Outbox payload shape | Server tests |
| `tools/*.schema.json` | What the LLM may request | The tool registry loads these at start-up |
| `fixtures/*.json` | Request, expected status, headers, body schema, and expected outbox events with idempotency keys | Conformance runner |
| `migrations/*.sql` | Database schema | The server |
| `RULES.md` | Everything else: key formats, transitions, tiers, timestamp and UUID formatting | People, and the fixtures that encode it |

The Go service is the reference implementation. `openapi.yaml` and the migrations live only in this repository.

Rules every client must respect:

- **Timestamps** are RFC 3339 in UTC with a `Z` suffix.
- **Field names** are snake_case in the Taskify API. (AgentPulse payloads are camelCase, but clients never see those.)
- **Updates carry a version.** Clients send `If-Match` and handle `409`.
- **Unknown fields** in a response are ignored, not treated as errors, so an older client survives a newer server.

### 9.3 Conformance suite

A black-box runner that knows only a URL:

```bash
TARGET_URL=http://localhost:8080 go test -v ./tests/conformance/...
```

```mermaid
flowchart LR
    fx["contract/fixtures"]
    suite["Conformance runner"]
    mockAP["Mock AgentPulse"]
    mockLLM["Mock LLM with scripted responses"]
    sut["Server at TARGET_URL"]
    pg[("PostgreSQL, fresh schema")]

    fx --> suite
    suite -->|"HTTP requests and signed webhooks"| sut
    sut --> pg
    sut -->|"notify and feedback"| mockAP
    sut -->|"chat completions"| mockLLM
    suite -->|"assert received calls"| mockAP
    suite -->|"load script"| mockLLM
```

Each fixture states the request, the expected status, headers, and response schema, and the outbox events the request must produce. Outbox assertions are made through `GET /admin/outbox` and through what the mock AgentPulse received, so the runner needs no database access.

| Group | Examples |
| --- | --- |
| Task API | CRUD, pagination, validation errors, `409` on stale version, tenant isolation, archive and restore |
| Transitions | Every cell of the matrix in section 4.3 |
| Events | Each change produces the expected events with the expected keys and recipients |
| Delivery | Mock AgentPulse returns 503 then 200: exactly one accepted notification; ten failures: dead letter |
| Webhooks | Valid signature accepted; bad signature and stale timestamp rejected |
| Auth | Missing, expired, and wrongly signed tokens; header fallback refused in production |
| Agents | Scripted Tier 1 call changes the task; Tier 2 call waits for approval; malformed output changes nothing |
| HITL | Apply, dismiss, double-apply, stale version |

With one server, the suite's job is to keep the server honest to the contract that the CLI, frontend, and SDK are generated from.

**If a second server is ever built (decision 12).** Go stays the reference. The same suite runs against both targets, and CI blocks any merge that changes business logic, transitions, or the API unless the suite passes on both containers. This is recorded so the rule exists before it is needed; no second server is planned.

### 9.4 Python client SDK

A typed Python package for scripts, data pipelines, notebooks, and external agent workflows. It contains no business logic: the server remains the only place where transactions, locking, the outbox, and agents live.

```text
sdk/python/
├── pyproject.toml
├── src/taskify/
│   ├── __init__.py
│   ├── _generated/          # Pydantic models generated from openapi.yaml, never hand-edited
│   ├── client.py            # TaskifyClient (sync) and AsyncTaskifyClient
│   ├── resources/           # tasks.py, comments.py, agent_jobs.py, admin.py, notifications.py
│   ├── errors.py            # typed exceptions
│   ├── pagination.py        # iterators over cursor pages
│   └── auth.py              # token providers
└── tests/
```

| Topic | Design |
| --- | --- |
| Generation | Models from `openapi.yaml` with `datamodel-code-generator`; a thin hand-written client on `httpx` on top. CI regenerates and fails if the committed models differ. |
| Python version | 3.10 or later |
| Sync and async | Both, sharing the same resource definitions |
| Authentication | Bearer token passed in, or read from `TASKIFY_TOKEN`; a callable can supply refreshed tokens |
| Errors | One exception per error code in section 5.4: `NotFoundError`, `VersionConflictError` (carries the current version), `ValidationError`, `AuthError`, `RateLimitError` |
| Versions | `update()` takes the task object and sends its `version` as `If-Match`. No silent retry on conflict. |
| Pagination | `client.tasks.list(...)` returns an iterator that follows cursors |
| Retries | Idempotent requests (`GET`) retry on connection errors, 429, and 503 with backoff. Writes do not retry automatically. |
| Async jobs | `client.tasks.decompose(id)` returns a job; `job.wait(timeout=...)` polls until it leaves `pending` and `running` |
| Compatibility | Sends its version in `User-Agent`; ignores unknown response fields |
| Distribution (decision 21) | For M15, installed from a Git tag and subdirectory. No public wheels until v1.0, when the REST contract is stable; until then releases are GitHub Releases or internal wheels only. The distribution name is `taskify-sdk` and the import name is `taskify` (decision 26). Reserving `taskify-sdk` on the public index is done by the project owner. |

Install during M15:

```bash
pip install "git+https://github.com/rtrtarun/taskify.git@v0.1.0#subdirectory=sdk/python"
```

The SDK shares the repository's tags with the Go module, so an SDK release and a server release carry the same version number.

Usage:

```python
from taskify import TaskifyClient, VersionConflictError

client = TaskifyClient(base_url="https://taskify.example", token=token)

task = client.tasks.create(title="Fix checkout crash on staging", priority="p2_high")

for t in client.tasks.list(status="blocked"):
    print(t.id, t.title)

try:
    client.tasks.update(task, priority="p1_critical")
except VersionConflictError as e:
    task = client.tasks.get(task.id)   # someone else changed it; decide what to do
```

A Python agent built by another team uses this SDK like any other caller. It acts with its user's permissions through the public API and does not get the internal tool registry or bypass approval rules.

### 9.5 CLI: `taskctl`

Written in Go with `spf13/cobra` for commands and `spf13/viper` for configuration (decision 16), on top of `pkg/client`. It talks only to the REST API. It may import validation constants and error codes from `internal/domain`, since it lives in the same module, so priority names and status values are not duplicated.

**Command tree.**

```text
taskctl
├── login                         # store credentials for a profile (O2: until an identity provider is chosen, it stores a token pasted from `cmd/devtoken`)
├── config
│   ├── view
│   ├── set <key> <value>
│   └── use-profile <name>
├── task
│   ├── add <title> [--desc --priority --assignee --due --label ...]
│   ├── list [--status --priority --assignee --mine --limit]
│   ├── show <id> [--comments --subtasks --agent-runs]
│   ├── update <id> [--title --desc --priority --assignee --due --status]
│   ├── done <id>
│   ├── block <id> [--reason]
│   ├── unblock <id>
│   ├── archive <id>
│   ├── comment <id> <text>
│   └── decompose <id> [--wait]
├── job
│   ├── list [--status awaiting_approval --task]
│   ├── show <id> [--watch]
│   ├── approve <id>
│   └── reject <id>
├── admin
│   ├── suggestions list
│   ├── suggestions decide <id> --approve | --reject
│   ├── rules bootstrap <workspace>
│   ├── outbox list [--status dead_letter]
│   └── outbox replay <id>
├── completion <shell>
└── version
```

**Behaviour.**

| Topic | Design |
| --- | --- |
| Output | Aligned table for people; `--json` prints the API response body unchanged for scripts; `--quiet` prints only IDs |
| Config | `config.yaml` in the user config directory, with named profiles: `server`, `tenant`, `workspace`, credential reference |
| Precedence | Flag, then environment variable (`TASKIFY_SERVER`, `TASKIFY_TOKEN`, `TASKIFY_PROFILE`), then profile |
| Credentials | Stored in the operating system keychain where available, otherwise a file readable only by the user. Never echoed. |
| Versions | `update` reads the task, applies the change, and sends `If-Match`. On `409` it reports the conflict and does not retry blindly. |
| IDs | Accepts a full UUID or a unique prefix |
| Async commands | `decompose` returns the job ID at once; `--wait` polls until the job leaves `pending` and `running` |
| Compatibility | Sends its version in `User-Agent`; warns if the server's API version is newer than it understands |

**Exit codes.**

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Unexpected error |
| 2 | Usage error |
| 3 | Authentication or permission failure |
| 4 | Not found |
| 5 | Version conflict |
| 6 | Server unreachable |

**Example flow.**

```mermaid
sequenceDiagram
    autonumber
    participant U as User
    participant C as taskctl
    participant A as Taskify API

    U->>C: taskctl task update 0192f4 --priority p1_critical
    C->>C: load profile, resolve credentials
    C->>A: GET /api/v1/tasks filtered by id prefix 0192f4
    A-->>C: task at version 4
    C->>A: PATCH /api/v1/tasks/ID with If-Match 4
    alt 200
        A-->>C: task at version 5
        C-->>U: updated, exit 0
    else 409
        A-->>C: conflict, current version 5
        C-->>U: task changed, re-run to apply, exit 5
    end
```

ID-prefix lookup needs an `id_prefix` filter on the list endpoint; it is part of the M12 contract additions.

### 9.6 Web frontend

**Stack.** React with TypeScript, built with Vite. Server state through TanStack Query. Routing with React Router. API types generated from `openapi.yaml`, so a contract change that breaks the UI fails the type check. These are suggested choices; nothing in the backend depends on them.

**Structure.**

```mermaid
flowchart TB
    subgraph browser["Browser"]
        subgraph pagesG["Pages"]
            tasksP["Task list and board"]
            detailP["Task detail"]
            inboxP["Approvals inbox"]
            briefP["Morning brief"]
            adminP["Admin"]
        end
        subgraph sharedG["Shared"]
            comps["UI components"]
            depGraph["Dependency graph view"]
            timeline["Agent activity timeline"]
        end
        subgraph dataG["Data layer"]
            queryC["Query cache"]
            apiC["Typed API client, generated"]
            authC["Auth and session"]
        end
    end
    api["Taskify API"]

    pagesG --> sharedG
    pagesG --> queryC
    queryC --> apiC
    apiC --> authC
    apiC -->|"REST /api/v1"| api
```

```text
web/
├── package.json
├── src/
│   ├── api/            # generated types and client
│   ├── auth/
│   ├── pages/          # tasks, task-detail, approvals, brief, admin
│   ├── components/
│   ├── hooks/
│   └── main.tsx
└── e2e/                # browser tests
```

**Screens.**

| Screen | Shows | Endpoints | Needs |
| --- | --- | --- | --- |
| Task list | Filterable, paginated list; quick add | `GET /tasks`, `POST /tasks` | R1 |
| Board | Columns by status; drag to change status | `GET /tasks`, `PATCH /tasks/{id}` | R1 |
| Task detail | Fields, comments, watchers, feedback actions | `GET /tasks/{id}`, `PATCH`, `POST .../comments`, `POST .../complete` | R1 |
| Subtasks and dependencies | Children and a graph of dependency links; Decompose button | `GET /tasks/{id}/subtasks`, `POST .../decompose` | M8 |
| Agent activity | Per-task timeline: which agent, what changed, reasoning, proposal status | `GET /tasks/{id}/agent-runs` | M5 |
| Approvals inbox | Proposals awaiting the user, with before and after values | `GET /agent-jobs?status=awaiting_approval`, `POST .../apply`, `POST .../dismiss` | M6 |
| Morning brief | Top three items, pending approvals, newly unblocked | Digest content | M10 |
| Notifications | In-app notification feed with act, snooze, dismiss | `GET /notifications/in-app`, `POST /notifications/{id}/ack` | M14 |
| Admin: learning | Suggestions with evidence; approve or reject | `GET /admin/learning/suggestions`, `POST .../decision` | R1 |
| Admin: delivery | Dead-letter rows with last error; replay | `GET /admin/outbox`, `POST .../replay` | M12 |
| Admin: agents | Job list, run history, token use | `GET /agent-jobs`, `GET /tasks/{id}/agent-runs` | M5 |

**Edit with conflict handling.**

```mermaid
sequenceDiagram
    autonumber
    participant U as User
    participant P as Task detail page
    participant Q as Query cache
    participant A as Taskify API

    U->>P: change priority
    P->>Q: optimistic update, keep previous value
    Q->>A: PATCH /api/v1/tasks/ID with If-Match version
    alt 200
        A-->>Q: task with new version
        Q-->>P: confirm
    else 409
        A-->>Q: conflict
        Q->>Q: roll back to previous value
        Q->>A: GET /api/v1/tasks/ID
        A-->>Q: latest task
        Q-->>P: show latest, tell the user their change was not saved
    end
```

**Design points.**

| Topic | Design |
| --- | --- |
| Authentication | JWT, per decision 2. Sign-in redirects to the identity provider; the token is held in memory with a refresh cookie that is `HttpOnly`, not in local storage. The identity provider is not chosen (O2, decision 38): until it is, the app accepts a development token from `cmd/devtoken`, and the redirect flow is built when O2 is resolved. |
| Tenant and role | Read from the token. Admin pages are hidden for non-admins, and the server still enforces access. |
| Live updates | Phase 1: polling every 15 to 30 seconds with conditional requests. Phase 2: Server-Sent Events. No WebSockets. Detail below. |
| Agent changes | Always labelled with the agent name and reasoning from `agent_metadata`. A proposal shows current and proposed values side by side. |
| Untrusted text | Task and comment text is rendered as text, never as HTML. Markdown, if added, goes through a sanitiser. Agent-written text is treated the same way. |
| Errors | Each API error code in section 5.4 maps to a specific message; `409` gets the reload flow above. |
| Accessibility | Keyboard reachable, visible focus, labelled controls. Board drag-and-drop has a non-drag alternative. |
| Layout | Usable at phone width for list, detail, and approvals. Admin pages may assume a wider screen. |

**Backend changes the frontend requires.**

- CORS: allowed origins from a `CORS_ALLOWED_ORIGINS` variable. Not needed if the frontend is served from the same origin.
- The list endpoints added in section 5.4 (R3 rows).
- A `GET /api/v1/me` endpoint returning the current user, tenant, and role.
- Consistent error bodies: `{"error": {"code": "...", "message": "...", "details": {...}}}`, defined once in `openapi.yaml`.
- Rate limits that allow for polling.
- The notification feed proxy, conditional requests, and optional embedded serving described below.

**In-app notification feed (decision 13).** The browser never talks to AgentPulse. It holds only a Taskify JWT.

```mermaid
sequenceDiagram
    autonumber
    participant W as Web frontend
    participant T as Taskify API
    participant AP as AgentPulse
    participant DB as PostgreSQL

    W->>T: GET /api/v1/notifications/in-app with user JWT
    T->>T: verify JWT, take tenant and user from the token
    T->>AP: GET in-app feed for tenant and user, with service credentials
    alt AgentPulse answers
        AP-->>T: raw notifications
        T->>DB: load referenced tasks and local ack state
        T-->>W: notifications with task link, assignee, labels, read flag
    else timeout or breaker open
        T-->>W: 503 with a retry hint, the rest of the page keeps working
    end
    W->>T: POST /api/v1/notifications/ID/ack with signal acted or dismissed
    T->>DB: record ack
    T->>AP: POST /feedback
    T-->>W: 200
```

- The AgentPulse feed endpoint is `GET /v1/tenants/{tenantId}/users/{userId}/in-app-feed?limit=&cursor=`, called with the same bearer key as the rest of the `/v1/` API (section 5.5; AgentPulse slice 40d). It is verified against a real AgentPulse in M0-07.
- Tenant and user in that call come from the verified token, never from the request, so one user cannot read another's feed.
- Ack state is stored in a small table, `notification_acks (tenant_id, user_id, notification_id, signal, acked_at)`, added in migration `000010` (M14-06). The ack is saved locally first; if forwarding the feedback to AgentPulse fails, it is retried through the outbox instead of failing the click.
- This is a synchronous call to AgentPulse on a read path. It has a short timeout and uses the circuit breaker. Task reads and writes do not depend on it.

**Live updates (decision 14).**

| Phase | Mechanism | Latency | When |
| --- | --- | --- | --- |
| 1 | Polling every 15 to 30 seconds with `ETag` and `If-None-Match`, and `GET /tasks?updated_since=<timestamp>` for deltas | 15 to 30 s | M14 |
| 2 | Server-Sent Events on `GET /api/v1/events` | Under a second | Later, when watching an agent work live matters |
| not used | WebSockets | | Two-way framing and reconnection logic are not needed for one-way updates |

- Phase 1: the `ETag` for a list is derived from a cheap query (row count and latest `updated_at` for the filter), so an unchanged list returns `304` without loading or serialising rows. It still costs one small query.
- Phase 2: the server fans out Postgres `LISTEN/NOTIFY` messages to connected clients, filtered by tenant and user. Taskify has no Redis of its own, so Postgres is the default source.
- Phase 2 authentication (decision 20): the frontend reads the stream with streaming `fetch` and the normal `Authorization: Bearer` header, for example through `@microsoft/fetch-event-source`, which also handles reconnection and backoff. The browser's built-in `EventSource` is not used because it cannot send that header. A JWT is never put in the URL.
- If a third-party client ever needs native `EventSource`, the fallback is a ticket exchange: `POST /api/v1/auth/sse-ticket` with bearer auth returns a random single-use ticket valid for 30 seconds, used as `/api/v1/events?ticket=...`. This is not built until someone needs it. Tickets are stored in Postgres, not Redis, so Taskify gains no new infrastructure:

  ```sql
  CREATE UNLOGGED TABLE sse_tickets (
      ticket_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
      tenant_id VARCHAR(64) NOT NULL,
      user_id VARCHAR(128) NOT NULL,
      expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 seconds')
  );
  CREATE INDEX idx_sse_tickets_expiry ON sse_tickets (expires_at);

  -- Consume: single use, rejected if no row comes back
  DELETE FROM sse_tickets
  WHERE ticket_id = $1 AND expires_at > NOW()
  RETURNING tenant_id, user_id;
  ```

  A background ticker deletes rows more than 5 minutes past expiry. An unlogged table is emptied after a database crash and is not copied to read replicas; both are acceptable for 30-second tickets, but the consume query must run on the primary.
- Archived tasks never appear in a normal list, so a delta poll alone would not tell the client that a task was archived. The `updated_since` response includes the IDs of tasks archived since that time.

**Deployment (decision 15).**

| Mode | How | Used for |
| --- | --- | --- |
| Separate | Vite `dist/` on an object store and CDN, or Nginx. Calls the API on its own origin, so `CORS_ALLOWED_ORIGINS` must list the frontend origin. | Production. A UI fix ships without rebuilding or restarting the Go service. |
| Embedded | The build is compiled into the Go binary with `go:embed` and served at `/` when `SERVE_EMBEDDED_SPA=true`. Same origin, so no CORS. | Local development, on-premises and self-hosted installs. |

Embedding notes:

- `go:embed` can only reference files at or below the directory of the Go file that declares it. The embed declaration therefore lives in `web/embed.go`, next to `web/dist/`, and `cmd/taskify` imports that package.
- A plain `go build` must not fail when the frontend has not been built. The embed sits behind a build tag (for example `-tags embedspa`), with a stub used otherwise.
- The file server falls back to `index.html` for unknown paths so client-side routes work, but never for paths under `/api/`.


### 9.7 Adding another language later

A new language means a new client SDK, not a new server.

1. Generate request and response types from `contract/openapi.yaml`.
2. Add a thin client: authentication, typed errors, cursor pagination, `If-Match` on updates.
3. Follow the client rules in section 9.2.
4. Test it against a running server, using the same scenarios as the Python SDK tests.
5. Add a CI job that regenerates the types and fails when they drift from the contract.

The Python SDK is the template. If a second server is ever proposed, section 9.3 and decision 12 describe the conditions.
