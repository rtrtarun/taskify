ALTER TABLE agent_jobs
    DROP CONSTRAINT IF EXISTS agent_jobs_conflict_count_non_negative,
    DROP COLUMN IF EXISTS conflict_count,
    DROP COLUMN IF EXISTS retry_after;
