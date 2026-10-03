-- 000006_create_audit_events.down.sql
-- DATA LOSS: the whole audit log.
BEGIN;
DROP TABLE IF EXISTS audit_events;
DROP FUNCTION IF EXISTS public.audit_events_guard();
COMMIT;
