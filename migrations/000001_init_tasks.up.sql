CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TYPE task_status AS ENUM ('todo', 'in_progress', 'blocked', 'completed', 'archived');
CREATE TYPE task_priority AS ENUM ('p1_critical', 'p2_high', 'p3_medium', 'p4_low');

-- A task can only be created in an existing workspace (decision 19).
CREATE TABLE workspaces (
    tenant_id  VARCHAR(64)  NOT NULL,
    id         VARCHAR(64)  NOT NULL,
    name       VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id)
);

CREATE TABLE tasks (
    -- IDs are UUIDv7 generated in Go; the default is a fallback only.
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 VARCHAR(64)   NOT NULL,
    workspace_id              VARCHAR(64)   NOT NULL,
    title                     VARCHAR(255)  NOT NULL,
    description               TEXT          NOT NULL DEFAULT '',
    status                    task_status   NOT NULL DEFAULT 'todo',
    priority                  task_priority NOT NULL DEFAULT 'p3_medium',
    creator_id                VARCHAR(128)  NOT NULL,
    assignee_id               VARCHAR(128),
    labels                    TEXT[]        NOT NULL DEFAULT '{}',
    metadata                  JSONB         NOT NULL DEFAULT '{}'::jsonb,
    due_at                    TIMESTAMPTZ,
    remind_at                 TIMESTAMPTZ,
    completed_at              TIMESTAMPTZ,
    last_due_soon_notified_at TIMESTAMPTZ,
    last_overdue_notified_at  TIMESTAMPTZ,
    archived_at               TIMESTAMPTZ,
    status_before_blocked     task_status,
    blocked_reason            VARCHAR(16),
    version                   INT           NOT NULL DEFAULT 1,
    created_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    -- RESTRICT: a workspace cannot be deleted while tasks reference it
    -- (decision 24).
    CONSTRAINT tasks_workspace_fk
        FOREIGN KEY (tenant_id, workspace_id)
        REFERENCES workspaces (tenant_id, id)
        ON DELETE RESTRICT,

    CONSTRAINT tasks_version_positive CHECK (version >= 1),

    CONSTRAINT tasks_blocked_reason_valid
        CHECK (blocked_reason IN ('dependency', 'manual')),

    -- The two block columns are set exactly when the task is blocked
    -- (decision 23).
    CONSTRAINT tasks_blocked_columns_match_status CHECK (
        (status = 'blocked') = (blocked_reason IS NOT NULL)
        AND (status = 'blocked') = (status_before_blocked IS NOT NULL)
    ),

    CONSTRAINT tasks_status_before_blocked_valid
        CHECK (status_before_blocked IN ('todo', 'in_progress'))
);

CREATE INDEX idx_tasks_tenant_assignee_status ON tasks (tenant_id, assignee_id, status);

CREATE INDEX idx_tasks_due_scan ON tasks (due_at, status)
    WHERE status NOT IN ('completed', 'archived') AND due_at IS NOT NULL;

CREATE TABLE task_watchers (
    task_id    UUID         NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    user_id    VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, user_id)
);

CREATE TABLE task_comments (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id    UUID         NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    author_id  VARCHAR(128) NOT NULL,
    body       TEXT         NOT NULL,
    mentions   TEXT[]       NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_task_comments_task ON task_comments (task_id, created_at);
