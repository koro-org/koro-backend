-- 000057_create_event_form_versions.down.sql
-- DATA LOSS: every published form version (registrations keep form_version_id
-- only until 000061 is reverted as well).
BEGIN;
DROP TABLE IF EXISTS event_form_versions;
COMMIT;
