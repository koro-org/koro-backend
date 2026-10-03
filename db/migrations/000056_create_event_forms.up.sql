-- 000056_create_event_forms.up.sql (02 §11.3). The registration form of an
-- event: one editable draft schema plus a pointer to the current published
-- version. current_version_id gets its FK in 000058, after
-- event_form_versions exists (circular reference). app_rw may delete the
-- row only so that a draft event can be deleted; the FK from
-- event_form_versions keeps any form that has a published version.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE event_forms (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  event_id            uuid NOT NULL CONSTRAINT event_forms_event_id_uq UNIQUE CONSTRAINT event_forms_event_id_fkey REFERENCES events (id),
  org_id              uuid NOT NULL CONSTRAINT event_forms_org_id_fkey REFERENCES organizations (id),
  draft_schema        jsonb NOT NULL DEFAULT '{"schema_version":1,"fields":[]}',
  current_version_id  uuid,                 -- FK in 000058
  updated_by          uuid CONSTRAINT event_forms_updated_by_fkey REFERENCES users (id),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER event_forms_touch_updated_at BEFORE UPDATE ON event_forms
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON event_forms TO app_rw;
COMMIT;
