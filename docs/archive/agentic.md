Transforming Taskify into an agentic task system means moving from **passive storage** (waiting for users to type and check boxes) to an **autonomous execution loop** where background agents observe state changes, reason over workspace context, invoke deterministic tools, and proactively communicate via AgentPulse.

---

## 1. The Agentic Architecture: Perception, Reasoning & Action Loop

```
 ┌─────────────────────────────────────────────────────────────────────────────┐
 │                         TASKIFY AGENTIC PLANE                               │
 │                                                                             │
 │   1. PERCEPTION           2. REASONING ENGINE         3. DETERMINISTIC ACT  │
 │  ┌──────────────┐        ┌──────────────────┐        ┌──────────────────┐   │
 │  │ Domain Events│        │ Small LLM / SLM  │        │ Go Tool Registry │   │
 │  │ & Webhooks   │───────▶│ (Structured JSON │───────▶│ - UpdateTask     │   │
 │  │ (Outbox/Bus) │        │  Tool Calling)   │        │ - AddDependency  │   │
 │  └──────────────┘        └──────────────────┘        │ - DraftComment   │   │
 │         ▲                          │                 └──────────────────┘   │
 │         │                          ▼                          │             │
 │         │                ┌──────────────────┐                 │             │
 │         │                │ Human-in-the-Loop│                 ▼             │
 │         │                │ Approval Gate    │        ┌──────────────────┐   │
 │         │                └──────────────────┘        │   AgentPulse     │   │
 │         │                          │                 │   Notify /       │   │
 │         └──────────────────────────┴─────────────────│   Feedback       │   │
 │                                                      └──────────────────┘   │
 └─────────────────────────────────────────────────────────────────────────────┘

```

The system operates across four discrete stages:

1. **Perception (Event-Driven Ingestion):** The agent does not poll blindly. It consumes Taskify domain events (`task.created`, `task.blocked`, `task.comment_added`, `task.overdue`) directly from PostgreSQL via the Outbox or an internal event bus.
2. **Reasoning (Context Window Construction):** An agent worker constructs a strict JSON prompt containing the task metadata, recent comments, dependency graph, and user historical feedback. It passes this context to an LLM/SLM (e.g., local Ollama/vLLM or frontier API) using strict JSON Schema / Tool Calling.
3. **Action (Go Tool Execution):** The model cannot execute arbitrary mutations. It emits typed tool calls against a registered Go interface (`UpdateTaskPriority`, `CreateSubtasks`, `LinkDependencies`, `DraftResolutionComment`).
4. **Human-in-the-Loop & Notification Routing:** Destructive actions (reassigning ownership, archiving, deadline shifts greater than 24 hours) require user confirmation via an interactive AgentPulse notification with action payloads (`approve`/`reject`). Non-destructive enrichments (tagging, subtask drafts, dependency inference) execute immediately.

---

## 2. Agentic Features Specification

### Feature 1: Autonomous Triage & Contextual Priority Agent

* **Trigger:** `task.created` or `task.updated`.
* **Behavior:** When a user creates a bare task like *"fix checkout crash on staging"*, the agent:
1. Evaluates historical tasks, labels, and title semantics.
2. Synthesizes a structured description, tags (`bug`, `backend`, `p1_critical`), and estimates cognitive effort.
3. Detects if this task matches known active blockers and links them automatically.
4. Generates a calibrated priority score recommendation fed into AgentPulse's `priorityHint`.



### Feature 2: Proactive Blocker Remediation & Stale Task Investigator

* **Trigger:** Task status transitions to `blocked`, or a task stays `in_progress` without comments for 48 hours.
* **Behavior:**
1. The agent inspects the blocking reason from comments or metadata.
2. Identifies dependencies (e.g., waiting on another user or API endpoint).
3. Prepares a succinct "Blocker Resolution Brief" with diagnostic steps or links to related PRs/tickets.
4. Fires an escalated AgentPulse alert to the blocker's owner with a 1-click action: *"Unblock & set In Progress"*.



### Feature 3: Natural Language Task Decomposition & Dependency Graph Generator

* **Trigger:** User issues a command like `/decompose` via comment, or sends a raw high-level requirement via API/CLI.
* **Behavior:**
1. Parses multi-step requirements into atomic, executable child tasks.
2. Establishes a Directed Acyclic Graph (DAG) using `parent_task_id` and `blocked_by_task_ids`.
3. Automatically schedules dependencies so dependent tasks only notify assignees when upstream blockers complete (`task.completed` triggers notification for downstream task ready).



### Feature 4: Cognitive Load Gatekeeper & Smart Digest Synthesizer

* **Integration Point:** Deep hook into AgentPulse Attention Intelligence and Digest queues.
* **Behavior:**
1. Evaluates user's pending workload (e.g., user already has 3 `p1_critical` tasks in progress).
2. If a new non-critical task is assigned, the agent automatically suppresses immediate notification, routes it to the user's daily digest, and adds a note: *"Deferred to avoid context switching"*.
3. Compiles an autonomous **Morning Brief** summarizing top 3 focal items, pending approvals, and unblocked tasks.



### Feature 5: Closed-Loop Notification Pruning Agent

* **Integration Point:** AgentPulse M38 Learning Feedback signals (`acted`, `dismissed`, `snoozed`, `muted`).
* **Behavior:**
1. Monitors persistent dismissal or muting patterns for specific task categories (e.g., user always dismisses `task.comment_added` on documentation tasks).
2. The agent generates an autonomous rule tuning proposal: *"Suppress in_app alerts for 'docs' label and batch to weekly digest"*.
3. Surfaces this directly in Taskify's admin panel as an M38 Learning Suggestion.



---

## 3. Prioritized Engineering Backlog

| ID | Priority | Epic | Feature / Deliverable | Technical Scope & Components | Complexity |
| --- | --- | --- | --- | --- | --- |
| **AG-101** | **P0** | **Agent Runtime** | Agent Worker Loop & Tool Execution Engine | Dedicated Go worker pool reading from `agent_jobs` queue; interfaces for LLM inference (Ollama/OpenAI-compatible) and deterministic Go tool registry. | `M` (3 days) |
| **AG-102** | **P0** | **Domain Model** | Dependency DAG & Agent Metadata Schema | Add `parent_task_id`, `blocked_by []UUID`, `agent_metadata JSONB`, and `agent_run_history` tables to PostgreSQL. | `S` (1 day) |
| **AG-103** | **P0** | **Triage Agent** | Auto-Tagging, Description & Priority Inference | Background agent triggered on `task.created`; uses structured tool calling to auto-assign priority, labels, and estimated effort. | `M` (2 days) |
| **AG-104** | **P1** | **Decomposer** | Natural Language Subtask Synthesizer | `/decompose` endpoint and comment trigger; generates dependency-linked subtasks with atomic database transaction. | `M` (2-3 days) |
| **AG-105** | **P1** | **Blocker Agent** | Proactive Blocker Resolution & Nudge Loop | Background cron evaluating stalled/blocked tasks; drafts resolution steps and fires high-priority AgentPulse interactive notifications. | `M` (2 days) |
| **AG-106** | **P1** | **HITL Bridge** | Human-in-the-Loop Webhook Action Handler | Inbound webhook handler for AgentPulse callbacks validating HMAC signatures to apply/reject agent-proposed changes. | `S` (1-2 days) |
| **AG-107** | **P2** | **Digest Agent** | Attention-Aware Morning/Evening Briefs | Aggregates daily tasks, filters by user cognitive budget, and formats a synthesized Markdown briefing via AgentPulse digest channel. | `M` (2 days) |
| **AG-108** | **P2** | **Learning Agent** | Notification Pruning & M38 Rule Suggestion | Queries AgentPulse `/feedback` analytics, aggregates mute/dismiss ratios, and generates automated M38 score/rule proposals. | `L` (3-4 days) |

---

## 4. Technical Architecture & Implementation Details

### A. Database Extensions (`migrations/000003_agent_schema.up.sql`)

```sql
-- Track dependencies and agent-specific state
ALTER TABLE tasks 
    ADD COLUMN parent_id UUID REFERENCES tasks(id) ON DELETE CASCADE,
    ADD COLUMN blocked_by UUID[] NOT NULL DEFAULT '{}',
    ADD COLUMN estimated_minutes INT,
    ADD COLUMN agent_metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX idx_tasks_blocked_by ON tasks USING GIN (blocked_by);

-- Queue for asynchronous agent reasoning jobs
CREATE TYPE agent_job_status AS ENUM ('pending', 'running', 'completed', 'failed');

CREATE TABLE agent_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    agent_type VARCHAR(64) NOT NULL, -- 'triage', 'decomposer', 'blocker_investigator', 'digest'
    status agent_job_status NOT NULL DEFAULT 'pending',
    input_context JSONB NOT NULL,
    proposed_actions JSONB,
    execution_result JSONB,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_jobs_poll ON agent_jobs (status, created_at) WHERE status = 'pending';

```

### B. Go Tool Registry Interface

Agents reason in JSON, but execution must remain strictly typed and validated:

```go
package agent

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
)

type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage // JSON Schema
	Execute(ctx context.Context, tenantID string, args json.RawMessage) (any, error)
}

// ToolRegistry holds deterministic functions the LLM can call
type ToolRegistry struct {
	tools map[string]Tool
}

// Example Tool: UpdateTaskAttributes
type UpdateTaskArgs struct {
	TaskID    uuid.UUID `json:"task_id"`
	Priority  *string   `json:"priority,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Reasoning string    `json:"reasoning"`
}

// Example Tool: CreateSubtaskDAG
type CreateSubtaskDAGArgs struct {
	ParentTaskID uuid.UUID `json:"parent_task_id"`
	Subtasks     []struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		DependsOn   []string `json:"depends_on"` // Titles of sibling subtasks
	} `json:"subtasks"`
}

```

### C. Human-in-the-Loop Safeguards

To prevent autonomous runaway:

1. **Safety Tier 1 (Autonomous):** Adding tags, updating description summaries, drafting comments, and linking non-blocking dependencies execute immediately and append to `agent_metadata`.
2. **Safety Tier 2 (Approval Required):** Modifying task priority (`p3` -> `p1`), changing assignees, or postponing due dates generates an `agent_jobs` record in `pending` state and sends an AgentPulse notification with two actionable URLs:
* `Approve`: `POST /api/v1/agent-jobs/{id}/apply`
* `Reject`: `POST /api/v1/agent-jobs/{id}/dismiss`



---

## 5. Master Prompt for Claude: Building the Agentic Plane

Use this self-contained prompt to implement the agentic plane inside your repository:

```markdown
You are a Principal AI & Distributed Systems Engineer in Go. We are extending `taskify` (integrated with `agentic-notification` / AgentPulse) to introduce an autonomous Agentic Task Plane.

### Objectives:
1. Implement the database schema migrations for task dependencies (`parent_id`, `blocked_by`), cognitive metrics (`estimated_minutes`), and the `agent_jobs` table.
2. Build an extensible, lightweight LLM orchestration package (`internal/agent/`) in pure Go supporting OpenAI/Ollama-compatible chat completion APIs with native Tool Calling (Function Calling).
3. Implement 3 core autonomous agents:
   - **TriageAgent:** Triggered on `task.created`; automatically suggests labels, estimates effort, and infers priority.
   - **DecomposerAgent:** Triggered by API `POST /api/v1/tasks/{id}/decompose` or comment command `/decompose`; breaks high-level tasks into an ordered subtask DAG.
   - **BlockerRemediationAgent:** Background worker scanning for tasks in `blocked` state; diagnoses blockers from context and prepares an AgentPulse interactive resolution alert.
4. Implement Human-in-the-Loop (HITL) execution controls:
   - High-impact mutations require approval via AgentPulse notification action webhooks.
   - Low-impact enrichments apply atomically with optimistic locking.
5. Provide comprehensive unit and integration tests with mock LLM providers verifying valid tool calling, fallback handling when the model produces malformed JSON, and transactional safety.

### Technical Guidelines:
- Use standard library `net/http` or minimal client for LLM API calls; enforce strict 30-second context timeouts and retry with exponential backoff on HTTP 429/503.
- All database state mutations must use transactions with `pgxpool`.
- Maintain complete compatibility with existing REST endpoints and the AgentPulse Outbox Relay.
- Write tests with `go test -race ./...` and ensure zero race conditions.

```