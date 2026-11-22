-- Morning brief (M10-02, M10-03; departure D37).

-- When the task last left 'blocked' for 'todo' or 'in_progress', whoever
-- lifted the block. The brief lists the tasks unblocked since the last one.
ALTER TABLE tasks ADD COLUMN unblocked_at TIMESTAMPTZ;

-- A person's briefs are digest jobs with no task, naming the person in
-- input_context. GET /api/v1/me/brief reads the newest.
CREATE INDEX idx_agent_jobs_brief ON agent_jobs (tenant_id, (input_context->>'user_id'), created_at DESC)
    WHERE agent_type = 'digest';
