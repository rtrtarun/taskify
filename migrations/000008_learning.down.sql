-- Suggestions waiting in the outbox have no record once the table is gone;
-- they are dropped with it, as the learning agent that wrote them is.
DELETE FROM notification_outbox WHERE event_type = 'learning.suggestion';
DROP TABLE IF EXISTS learning_suggestions;
