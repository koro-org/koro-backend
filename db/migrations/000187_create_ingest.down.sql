-- 000187_create_ingest.down.sql (BE-210).
-- DATA LOSS: every ingest source, run, imported club link and imported event.
-- The clubs created from the official lists stay (directory's rows). The
-- retention policy goes unless a destruction record refers to it.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE imported_events;
DROP TABLE imported_clubs;
DROP TABLE ingest_runs;
DROP TABLE ingest_sources;
DELETE FROM public.retention_policies WHERE code = 'imported_event'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'imported_event');
COMMIT;
