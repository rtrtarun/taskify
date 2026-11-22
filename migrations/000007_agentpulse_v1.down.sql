DROP TABLE IF EXISTS webhook_callbacks;
-- Contact forwards waiting in the outbox have nothing to send once the
-- table is gone.
DELETE FROM notification_outbox WHERE event_type = 'contact.update';
DROP TABLE IF EXISTS user_contacts;
