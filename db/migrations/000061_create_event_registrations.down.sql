-- 000061_create_event_registrations.down.sql
-- DATA LOSS: every event registration with its answers.
BEGIN;
DROP TABLE IF EXISTS event_registrations;
COMMIT;
