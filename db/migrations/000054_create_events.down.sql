-- 000054_create_events.down.sql
-- DATA LOSS: every event.
BEGIN;
DROP TABLE IF EXISTS events;
DROP FUNCTION IF EXISTS public.events_forbid_nondraft_delete();
COMMIT;
