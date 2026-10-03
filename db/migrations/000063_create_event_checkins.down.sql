-- 000063_create_event_checkins.down.sql
-- DATA LOSS: every check-in record (attendance of all events).
BEGIN;
DROP TABLE IF EXISTS event_checkins;
COMMIT;
