-- 000058_add_fk_event_forms_current_version.up.sql (02 §11.4). The circular
-- reference of 000056: event_forms.current_version_id points at the version
-- published last.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE event_forms ADD CONSTRAINT event_forms_current_version_id_fkey
  FOREIGN KEY (current_version_id) REFERENCES event_form_versions (id);
COMMIT;
