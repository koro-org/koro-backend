-- 000058_add_fk_event_forms_current_version.down.sql
BEGIN;
ALTER TABLE event_forms DROP CONSTRAINT IF EXISTS event_forms_current_version_id_fkey;
COMMIT;
