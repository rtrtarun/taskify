-- Default AgentPulse routing rules registered for each workspace. One row per
-- rule makes bootstrap-rules safe to repeat, including after a partial failure.
CREATE TABLE workspace_rules (
    tenant_id          VARCHAR(64)  NOT NULL,
    workspace_id       VARCHAR(64)  NOT NULL,
    rule_key           VARCHAR(64)  NOT NULL,
    agentpulse_rule_id VARCHAR(128) NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, workspace_id, rule_key),
    FOREIGN KEY (tenant_id, workspace_id) REFERENCES workspaces (tenant_id, id) ON DELETE CASCADE
);

-- Signatures of accepted AgentPulse webhook callbacks. A byte-identical replay
-- carries the same signature and is ignored. Rows older than the replay
-- window are useless and are deleted by the cleanup worker.
CREATE TABLE webhook_deliveries (
    signature   VARCHAR(128) PRIMARY KEY,
    received_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_webhook_deliveries_received ON webhook_deliveries (received_at);
