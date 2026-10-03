-- 000055_create_event_staff.down.sql
-- DATA LOSS: the door staff of every event.
BEGIN;
DROP TABLE IF EXISTS event_staff;
COMMIT;
