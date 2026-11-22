-- Indexes for the list endpoints the clients use (M12-04): each reads one
-- tenant's rows newest first, a page at a time, continuing after a
-- (created_at, id) cursor.

-- GET /api/v1/agent-jobs, which the approvals inbox polls.
CREATE INDEX idx_agent_jobs_tenant_created ON agent_jobs (tenant_id, created_at DESC, id DESC);

-- GET /api/v1/admin/outbox, usually filtered to dead_letter.
CREATE INDEX idx_outbox_tenant_status_created ON notification_outbox (tenant_id, status, created_at DESC, id DESC);
