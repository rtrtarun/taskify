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
