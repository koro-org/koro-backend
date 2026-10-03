-- 000056_create_event_forms.down.sql
-- DATA LOSS: the form drafts of every event.
BEGIN;
DROP TABLE IF EXISTS event_forms;
COMMIT;
