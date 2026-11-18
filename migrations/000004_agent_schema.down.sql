DROP TABLE IF EXISTS agent_run_history;
DROP TABLE IF EXISTS agent_jobs;
DROP TYPE IF EXISTS agent_job_status;

ALTER TABLE workspaces DROP COLUMN IF EXISTS agent_settings;

DROP TABLE IF EXISTS task_dependencies;

DROP INDEX IF EXISTS idx_tasks_parent;
ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_parent_not_self,
    DROP CONSTRAINT IF EXISTS tasks_estimated_minutes_positive,
    DROP COLUMN IF EXISTS agent_metadata,
    DROP COLUMN IF EXISTS estimated_minutes,
    DROP COLUMN IF EXISTS parent_id;
