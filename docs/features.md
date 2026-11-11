# Taskify: What We Are Building and Why

Companion documents: [architecture.md](architecture.md) (how it is built) and [milestones.md](milestones.md) (in what order). Source material: [initial.md](initial.md) and [agentic.md](agentic.md).

## The product in one paragraph

Taskify today is a starter Go project: a `main.go` that prints a greeting and a half-written router. We are turning it into a multi-tenant task service that does two things ordinary task managers do not. First, it hands every notification decision to **AgentPulse** (the `agentic-notification` platform), so users get one alert for the thing that matters instead of twenty alerts for everything. Second, it runs **background AI agents** that tidy, split, and unblock tasks on their own, asking a human only before changes that are hard to undo.

People reach it through three front doors: the REST API, a **command-line tool**, and a **web frontend**. All three are generated from one **language-neutral contract**. A **Python client SDK** is generated from the same contract, and SDKs for other languages can follow the same way. There is one server, written in Go.

## Before and after

| Area | Today | After Layer 1 (attention-aware) | After Layer 2 (agentic) |
| --- | --- | --- | --- |
| Task data | Nothing persisted | Tenant-scoped tasks with status, priority, assignee, watchers, comments, due dates | Adds subtasks, dependencies, effort estimates, agent notes |
| Notifications | None | Every task change becomes an event routed by AgentPulse: deduplicated, budgeted, digested | Agents add priority hints and defer low-value alerts when a user is overloaded |
| Reliability | None | No lost or duplicated notifications, even if AgentPulse is down | Agent work is queued, retried, and audited the same way |
| Learning | None | User reactions (`acted`, `dismissed`, `snoozed`, `muted`) tune future scoring, with admin approval | An agent turns dismissal patterns into concrete rule proposals |
| Human effort | Everything manual | Manual task entry, smart delivery | Agents triage, decompose, and chase blockers |

Layer 3 (clients and portability) does not change what the service does. It adds a CLI, a web frontend, and a Python client SDK on top.

## Layer 1: Attention-aware task service

These features come from [initial.md](initial.md). Together they make Taskify a reliable producer of task events and let AgentPulse decide who is told what, when, and on which channel.

### F1. Rich task model

- **What it is:** Tasks carry tenant, workspace, creator, assignee, watchers, status (`todo`, `in_progress`, `blocked`, `completed`, `archived`), priority (`p1_critical` to `p4_low`), labels, due and reminder times, free-form metadata, and a `version` number.
- **Why it helps:** AgentPulse can only route intelligently if events say who is involved and how urgent the task is. The `version` number stops two people from silently overwriting each other's edits.

### F2. Task lifecycle events

- **What it is:** Nine named events: `task.created`, `task.assigned`, `task.updated`, `task.priority_escalated`, `task.blocked`, `task.due_soon`, `task.overdue`, `task.completed`, `task.comment_added` (with `@mention` detection).
- **Why it helps:** One fixed vocabulary means AgentPulse rules, agents, and metrics all key off the same names. Adding a new consumer later needs no change to the task code.

### F3. Guaranteed, non-blocking notification delivery (transactional outbox)

- **What it is:** When a task changes, the change and its notification record are saved in a single database transaction. A background relay sends the record to AgentPulse later, retrying with backoff and parking it in a dead-letter state after 10 failures.
- **Why it helps:**
  - The API stays fast (target P95 under 15 ms for writes) because it never waits on AgentPulse.
  - An AgentPulse outage delays notifications instead of losing them or failing task saves.
  - Each event has a deterministic idempotency key, so a retry never produces a second notification.

### F4. Deadline and escalation scheduler

- **What it is:** A background ticker that finds tasks due within 24 hours or 1 hour (`task.due_soon`) and tasks past due (`task.overdue`), and writes events for them.
- **Why it helps:** Users are reminded without setting reminders. Multiple Taskify replicas can run the scheduler at once and still produce exactly one notification per task per window.

### F5. Noise suppression through AgentPulse

- **What it is:** AgentPulse applies user preferences, `payloadMatch` rules, an attention score, and a per-user attention budget. Taskify registers three default rules per workspace: critical or blocked tasks go out immediately on Slack, email, and in-app; assignments and mentions go to in-app and email; `p4_low` goes to the daily digest.
- **Why it helps:** When 20 tasks go overdue together, the user gets the one `p1_critical` alert now and the other 19 in the next digest. This is the main difference from Todoist, Linear, TickTick, and Jira, whose notification settings are static.

### F6. Two-way feedback and action bridge

- **What it is:** Taskify forwards user reactions to AgentPulse (`POST /api/v1/tasks/{id}/feedback`) and accepts signed callbacks from AgentPulse (`POST /api/v1/webhooks/agentpulse`) when a user clicks "Mark done" or "Snooze 2h" inside a notification.
- **Why it helps:** Users act on a task from Slack or email without opening Taskify, and every reaction becomes training data for scoring.

### F7. Admin review of learned tuning

- **What it is:** Taskify endpoints that proxy AgentPulse's rules, preferences, digests, and M38 learning suggestions, so a workspace admin can approve or reject score adjustments.
- **Why it helps:** The system adapts to how a team works, but only within a ±20 score bound and only after a human approves. Nothing changes silently.

### F8. Operability

- **What it is:** `/healthz`, `/readyz`, Prometheus `/metrics`, OpenTelemetry traces that follow a request from the API through the outbox to AgentPulse, and a circuit breaker on the AgentPulse client.
- **Why it helps:** When a notification is late, one trace shows where it stalled. Dashboards show outbox backlog and dead-letter counts before users notice.

## Layer 2: Agentic task plane

These features come from [agentic.md](agentic.md). Agents react to task events, ask a language model what to do, and act only through a fixed list of typed Go tools.

### How agents are kept safe

| Tier | Examples | What happens |
| --- | --- | --- |
| Tier 1: autonomous | Add labels, improve a description, set an effort estimate, draft a comment, link a dependency | Applied immediately and recorded in the task's `agent_metadata` |
| Tier 2: approval required | Change priority, change assignee, move a due date, archive | Stored as a proposal. The user gets an AgentPulse notification with Approve and Reject actions. Nothing changes until they approve. |

The model never touches the database. It can only request one of the registered tools, and each request is validated against a schema before it runs.

### A1. Triage agent

- **Trigger:** A task is created or updated by a person.
- **What it does:** Takes a bare task such as "fix checkout crash on staging", writes a structured description, adds labels (`bug`, `backend`), estimates effort in minutes, links it to a matching active blocker if one exists, and recommends a priority.
- **Why it helps:** People can capture tasks in five words and still get well-formed, searchable, correctly routed tasks. The priority recommendation feeds AgentPulse's `priorityHint`, so urgent work is noticed even when the author forgot to mark it.

### A2. Blocker and stale-task investigator

- **Trigger:** A task moves to `blocked`, or sits `in_progress` for 48 hours with no comments.
- **What it does:** Reads the comments and dependencies to work out what the task is waiting on, writes a short "Blocker Resolution Brief", and sends the owner of the blocker an escalated notification with a one-click "Unblock and set In Progress" action.
- **Why it helps:** Blocked work usually stalls because the person who can fix it does not know. This closes that gap without a stand-up meeting.

### A3. Task decomposer

- **Trigger:** A `/decompose` comment, or `POST /api/v1/tasks/{id}/decompose`.
- **What it does:** Splits a high-level requirement into small child tasks and records which ones depend on which, forming a dependency graph with no cycles. A downstream assignee is notified only when the upstream tasks are complete.
- **Why it helps:** Planning time drops, and nobody is pinged about work they cannot start yet.

### A4. Cognitive load gatekeeper and digest writer

- **Trigger:** A new assignment, and a morning schedule.
- **What it does:** Checks the assignee's current load. If they already have, for example, three `p1_critical` tasks in progress and the new task is not critical, the notification is moved to their digest with the note "Deferred to avoid context switching". Each morning it writes a brief: top three items to focus on, approvals waiting, and tasks that became unblocked.
- **Why it helps:** Protects focus for the busiest people and replaces a scattered inbox with one prioritised summary.

### A5. Notification pruning agent

- **Trigger:** A scheduled analysis of AgentPulse feedback data.
- **What it does:** Looks for patterns such as "this user always dismisses comment alerts on `docs` tasks" and drafts a rule change, for example "suppress in-app alerts for the `docs` label and batch them weekly". The proposal appears in the admin panel as an M38 learning suggestion.
- **Why it helps:** Notification settings improve from real behaviour, and admins review a ready-made proposal instead of hand-writing rules.

## Layer 3: Clients and portability

These features were added after the two source documents. They make Taskify usable by people (not only by API callers) and callable from more than one programming language.

### C1. Language-neutral contract

- **What it is:** A `contract/` folder that is the single definition of Taskify's API: an OpenAPI file for the REST endpoints, JSON Schemas for event payloads and agent tools, and a written list of rules (status transitions, safety tiers, formats). A conformance test suite runs against the server over HTTP and checks it against the contract.
- **Why it helps:** The CLI, the frontend, and every SDK are generated from one file instead of drifting apart. Supporting another language means generating another client, which takes days, not rebuilding the service.

### C2. Python client SDK

- **What it is:** A typed Python package for calling Taskify: create and update tasks, page through lists, comment, request a decomposition and wait for it, approve proposals. Its data models are generated from the contract.
- **Why it helps:**
  - Scripts, data pipelines, notebooks, and Python-based agent workflows can use Taskify without hand-writing HTTP calls.
  - Errors are typed. A version conflict, for example, arrives as a specific exception carrying the current version.
  - It costs about two days to build and almost nothing to keep in step, because it is regenerated when the contract changes.
- **What it is not:** a second server. We considered a full Python implementation and decided against it: every transaction, locking rule, worker, and migration would have had to be built and kept in sync twice.

### C3. Command-line tool (`taskctl`)

- **What it is:** A single binary for working with Taskify from a terminal: create, list, update, complete, and comment on tasks; request a decomposition; approve or reject agent proposals; and, for admins, review learning suggestions and replay failed notifications. Output is a readable table by default and JSON with `--json`.
- **Why it helps:** Developers manage tasks without leaving the terminal. Scripts and CI jobs can create and close tasks. Operators get a quick way to inspect the dead-letter queue and agent jobs without writing `curl` commands.

### C4. Web frontend

- **What it is:** A browser application with a task list and board, a task detail page (comments, subtasks, dependency graph, and a timeline of what agents did and why), an in-app notification feed, an approvals inbox for agent proposals, the morning brief, and admin pages for learning suggestions, rules, agent run history, and failed notifications.
- **Why it helps:** Most users will never call an API. The frontend is also where the agent layer becomes trustworthy: people can see each agent change with its reasoning, and approve or reject proposals in one place instead of only from a notification.

## Who benefits

| Person | What changes for them |
| --- | --- |
| Everyday user | A web app for tasks, approvals, and the morning brief. |
| Developer | A terminal tool and a scriptable JSON interface. |
| Python user | A typed SDK for scripts, pipelines, and agent workflows. |
| Future maintainers | A contract to generate the next language's SDK from, and a suite that keeps the server true to it. |
| Assignee | Fewer, better-timed alerts. Tasks arrive already described and labelled. A morning brief instead of an inbox. |
| Task creator | Can write a one-line task and let triage fill in the rest. Hears when it is done. |
| Team lead | Blocked and stale work is escalated automatically. Large tasks are broken down with dependencies. |
| Workspace admin | Reviews tuning suggestions instead of maintaining notification schemes. Every agent action is logged. |
| On-call engineer | Metrics, traces, and a dead-letter queue for anything that failed to deliver. |

## Out of scope

- A native mobile app. The web frontend is responsive, but there is no iOS or Android client.
- A second server implementation in Python or any other language. Other languages get a client SDK.
- SDKs for languages other than Python. The contract makes them cheap to add; none is planned yet.
- Changes to AgentPulse itself. Taskify integrates with its existing contract.
- Agents executing arbitrary code or calling external systems. They can only call the registered Taskify tools.
- Fully autonomous high-impact changes. Tier 2 actions always wait for a person.

## Glossary

| Term | Meaning |
| --- | --- |
| AgentPulse | The `agentic-notification` platform that scores, routes, deduplicates, and batches notifications |
| Outbox | A database table of events waiting to be sent, written in the same transaction as the change that caused them |
| Idempotency key | A stable identifier for an event, so sending it twice has the same effect as sending it once |
| Attention budget | A per-user limit in AgentPulse on how many immediate notifications they receive in a period |
| Digest | A scheduled batch of lower-priority notifications |
| `payloadMatch` rule | An AgentPulse rule that routes a notification based on fields in its payload |
| M38 learning suggestion | An AgentPulse proposal to adjust scoring, generated from user feedback and applied only after admin approval |
| HITL | Human in the loop: a person approves an agent's proposal before it takes effect |
| DAG | Directed acyclic graph: tasks linked by "blocked by" relationships with no circular dependencies |
| Contract | The language-neutral definition of Taskify's API, events, tools, schema, and rules, kept in `contract/` |
| OpenAPI | A standard file format describing a REST API, from which clients and documentation can be generated |
| Conformance suite | Tests that run against the Taskify server over HTTP and check it against the contract |
| SDK | A library that wraps the API for one programming language, so callers use functions and typed objects instead of raw HTTP |
| Tool | A typed Go function an agent is allowed to request, such as `UpdateTaskAttributes` |
