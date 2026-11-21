-- Retry pacing for agent jobs (departure D24, closing G24).
--
-- retry_after: a pending job is not claimed before this time. Failed runs
-- back off (30s, 2m, 10m) and version conflicts wait briefly, so a model
-- outage or a task under active editing cannot spin the runtime.
-- conflict_count: version conflicts so far. The first few re-run without
-- using an attempt; after that each conflict counts as one.
ALTER TABLE agent_jobs
    ADD COLUMN retry_after    TIMESTAMPTZ,
    ADD COLUMN conflict_count INT NOT NULL DEFAULT 0,
    ADD CONSTRAINT agent_jobs_conflict_count_non_negative CHECK (conflict_count >= 0);
