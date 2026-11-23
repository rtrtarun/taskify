-- What each person did about an in-app notification (M14-06). The feed
-- itself lives in AgentPulse; Taskify keeps only the person's reaction, so it
-- can show the read state and forward one feedback signal per reaction.
--
-- One row per person and notification. A later, different reaction replaces
-- the row. snooze_duration and snooze_until are set only for 'snoozed'.
CREATE TABLE notification_acks (
    tenant_id       VARCHAR(64)  NOT NULL,
    user_id         VARCHAR(128) NOT NULL,
    notification_id VARCHAR(128) NOT NULL,
    signal          VARCHAR(16)  NOT NULL,
    snooze_duration VARCHAR(16),
    snooze_until    TIMESTAMPTZ,
    acked_at        TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (tenant_id, user_id, notification_id),
    CONSTRAINT notification_acks_signal_valid CHECK (signal IN ('acted', 'dismissed', 'snoozed')),
    CONSTRAINT notification_acks_snooze_columns CHECK (
        (signal = 'snoozed') = (snooze_duration IS NOT NULL)
        AND (signal = 'snoozed') = (snooze_until IS NOT NULL)
    )
);
