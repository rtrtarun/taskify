DROP INDEX IF EXISTS idx_agent_jobs_brief;
ALTER TABLE tasks DROP COLUMN IF EXISTS unblocked_at;
