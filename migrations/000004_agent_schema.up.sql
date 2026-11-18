-- R2 agent schema (architecture sections 4.1, 4.2, and 7).

-- Subtasks, estimates, and the record of agent changes.
-- parent_id is SET NULL, not CASCADE: a permanent delete of a parent must not
-- silently remove children, which would bypass outbox scrubbing, agent job
-- cleanup, and dependency resolution for each of them (departure D15).
ALTER TABLE tasks
    ADD COLUMN parent_id         UUID REFERENCES tasks (id) ON DELETE SET NULL,
    ADD COLUMN estimated_minutes INT,
    ADD COLUMN agent_metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT tasks_estimated_minutes_positive CHECK (estimated_minutes > 0),
    ADD CONSTRAINT tasks_parent_not_self CHECK (parent_id <> id);

CREATE INDEX idx_tasks_parent ON tasks (parent_id) WHERE parent_id IS NOT NULL;

-- One row means task_id cannot start until depends_on_task_id is completed or
-- archived (decisions 4 and 17). The foreign keys do not prove both tasks
-- share a tenant; the service checks that before insert.
CREATE TABLE task_dependencies (
    task_id            UUID        NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    depends_on_task_id UUID        NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    tenant_id          VARCHAR(64) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, depends_on_task_id),
    CONSTRAINT task_dependencies_not_self CHECK (task_id <> depends_on_task_id)
);

CREATE INDEX idx_task_deps_reverse ON task_dependencies (tenant_id, depends_on_task_id);

-- Workspace agent controls (decisions 10 and 19): agents are opt-in.
ALTER TABLE workspaces
    ADD COLUMN agent_settings JSONB NOT NULL DEFAULT
        '{"agents_enabled": false, "disabled_agents": [], "require_hitl_for_all": true, "max_subtasks_per_decomposition": 5}'::jsonb;

-- awaiting_approval and dismissed are separate from pending so the worker poll
-- never re-claims a job that is waiting on a person (section 7).
CREATE TYPE agent_job_status AS ENUM (
    'pending', 'running', 'awaiting_approval', 'completed', 'failed', 'dismissed'
);

CREATE TABLE agent_jobs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         VARCHAR(64)      NOT NULL,
    -- Nullable: digest and learning jobs are scoped to a user or tenant.
    task_id           UUID REFERENCES tasks (id) ON DELETE CASCADE,
    agent_type        VARCHAR(64)      NOT NULL,
    status            agent_job_status NOT NULL DEFAULT 'pending',
    dedup_key         VARCHAR(320)     NOT NULL UNIQUE,
    base_task_version INT,
    input_context     JSONB            NOT NULL DEFAULT '{}'::jsonb,
    proposed_actions  JSONB,
    execution_result  JSONB,
    error_message     TEXT,
    attempts          INT              NOT NULL DEFAULT 0,
    decided_by        VARCHAR(128),
    decided_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ      NOT NULL DEFAULT NOW(),

    CONSTRAINT agent_jobs_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT agent_jobs_decision_complete CHECK ((decided_by IS NULL) = (decided_at IS NULL))
);

CREATE INDEX idx_agent_jobs_poll ON agent_jobs (status, created_at) WHERE status = 'pending';

-- Recovery of jobs abandoned in 'running' (default G11).
CREATE INDEX idx_agent_jobs_running ON agent_jobs (updated_at) WHERE status = 'running';

CREATE INDEX idx_agent_jobs_task ON agent_jobs (task_id) WHERE task_id IS NOT NULL;

-- One row per model turn, for audit (section 6.4).
CREATE TABLE agent_run_history (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id            UUID         NOT NULL REFERENCES agent_jobs (id) ON DELETE CASCADE,
    turn              INT          NOT NULL,
    model             VARCHAR(128) NOT NULL DEFAULT '',
    tool_name         VARCHAR(64),
    tool_args         JSONB,
    tool_result       JSONB,
    outcome           VARCHAR(32)  NOT NULL,
    prompt_tokens     INT          NOT NULL DEFAULT 0,
    completion_tokens INT          NOT NULL DEFAULT 0,
    latency_ms        INT          NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT agent_run_history_turn_positive CHECK (turn >= 1)
);

CREATE INDEX idx_agent_run_history_job ON agent_run_history (job_id, turn);
