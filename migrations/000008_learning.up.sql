-- Learning agent (M11).

-- The learning suggestions Taskify submitted to AgentPulse, one row per
-- suggestion, written with the learning.suggestion outbox row that sends
-- it. A job submits a pattern at most once however often it runs, and the
-- learning agent leaves a pattern alone for the 30-day window after one was
-- submitted, so a decision is not asked twice on the same signals. A NULL
-- label is the notifications that carry none; labels are never empty, so
-- the unique index can stand NULL in for ''.
CREATE TABLE learning_suggestions (
    id                  UUID         PRIMARY KEY,
    tenant_id           VARCHAR(64)  NOT NULL,
    job_id              UUID         NOT NULL REFERENCES agent_jobs (id) ON DELETE CASCADE,
    event_type          TEXT         NOT NULL,
    label               TEXT,
    current_adjustment  INT          NOT NULL CHECK (current_adjustment BETWEEN -20 AND 20),
    proposed_adjustment INT          NOT NULL CHECK (proposed_adjustment BETWEEN -20 AND 20),
    signal_count        INT          NOT NULL CHECK (signal_count >= 5),
    rationale           TEXT         NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL
);

CREATE UNIQUE INDEX idx_learning_suggestions_job_pattern
    ON learning_suggestions (job_id, event_type, COALESCE(label, ''));

CREATE INDEX idx_learning_suggestions_tenant ON learning_suggestions (tenant_id, created_at);
