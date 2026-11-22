-- AgentPulse v1 alignment (task group AP).

-- A person's contact details for AgentPulse (AP-05), set by the person
-- through PUT /api/v1/me/contact and forwarded through the outbox as one
-- contact.update row per person. A row with all three NULL is no contact.
-- updated_at is NULL until the first change.
CREATE TABLE user_contacts (
    tenant_id     VARCHAR(64)  NOT NULL,
    user_id       VARCHAR(128) NOT NULL,
    email         VARCHAR(254),
    slack_user_id VARCHAR(32),
    phone         VARCHAR(16),
    updated_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, user_id)
);

-- AgentPulse callbacks already applied, by the callbackId AgentPulse puts in
-- every callback (AP-06). Its delivery queue retries a click with the same
-- callbackId but a new timestamp and signature, so the signature record of
-- webhook_deliveries (D10) cannot recognise the retry. Kept for 7 days, the
-- life of an action link; the cleanup worker deletes older rows.
CREATE TABLE webhook_callbacks (
    tenant_id   VARCHAR(64)  NOT NULL,
    callback_id VARCHAR(35)  NOT NULL,
    received_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, callback_id)
);

CREATE INDEX idx_webhook_callbacks_received ON webhook_callbacks (received_at);
