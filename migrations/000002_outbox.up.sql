CREATE TYPE outbox_status AS ENUM ('pending', 'processing', 'dispatched', 'dead_letter');

CREATE TABLE notification_outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       VARCHAR(64)   NOT NULL,
    -- No foreign key: outbox rows are removed explicitly on a permanent
    -- delete, not by cascade.
    aggregate_id    UUID          NOT NULL,
    event_type      VARCHAR(64)   NOT NULL,
    -- Wider than the 192 in the source DDL: a key can carry a tenant ID, a
    -- task ID, an event type, and a recipient ID.
    idempotency_key VARCHAR(320)  NOT NULL UNIQUE,
    payload         JSONB         NOT NULL,
    trace_context   JSONB         NOT NULL DEFAULT '{}'::jsonb,
    status          outbox_status NOT NULL DEFAULT 'pending',
    attempts        INT           NOT NULL DEFAULT 0,
    max_attempts    INT           NOT NULL DEFAULT 10,
    next_attempt_at TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    -- Set when a relay worker claims the row, so a lease reaper can return
    -- rows abandoned in 'processing' to 'pending'.
    claimed_at      TIMESTAMPTZ,
    last_error      TEXT,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    dispatched_at   TIMESTAMPTZ
);

CREATE INDEX idx_outbox_poll ON notification_outbox (next_attempt_at, created_at)
    WHERE status = 'pending';

CREATE INDEX idx_outbox_aggregate ON notification_outbox (aggregate_id);

CREATE INDEX idx_outbox_leases ON notification_outbox (claimed_at)
    WHERE status = 'processing';
