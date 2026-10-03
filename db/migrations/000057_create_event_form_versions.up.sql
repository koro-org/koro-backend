-- 000057_create_event_form_versions.up.sql (02 §11.4). A published form
-- version: immutable (two layers, 02 §0.5: app_rw has SELECT/INSERT only and
-- koro_forbid_mutation blocks UPDATE/DELETE/TRUNCATE for everyone). The KVKK
-- lint is part of the row:
--   * a version with a 'blocked' field (ID numbers, card numbers, passwords)
--     cannot be published and has no override: lint_report->'blocked' must be
--     empty (event_form_versions_blocked_chk);
--   * a version with a 'warn_special' field needs a reasoned override, and
--     has_sensitive_fields must say so (…_sensitive_flag_chk), so the flag
--     cannot be left false to skip the override;
--   * lint_report has the documented shape (…_lint_report_chk).
-- The last three are additions to the 02 DDL (the text states the rules).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE event_form_versions (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  form_id               uuid NOT NULL CONSTRAINT event_form_versions_form_id_fkey REFERENCES event_forms (id),
  event_id              uuid NOT NULL CONSTRAINT event_form_versions_event_id_fkey REFERENCES events (id),
  version_no            integer NOT NULL CONSTRAINT event_form_versions_version_no_chk CHECK (version_no > 0),
  schema                jsonb NOT NULL,                 -- R-003 §2.5.1 shape; at most 20 fields; key ^[a-z][a-z0-9_]{1,40}$
  schema_sha256         bytea NOT NULL CONSTRAINT event_form_versions_schema_sha256_chk CHECK (octet_length(schema_sha256) = 32),
  field_count           smallint NOT NULL CONSTRAINT event_form_versions_field_count_chk CHECK (field_count BETWEEN 0 AND 20),
  has_sensitive_fields  boolean NOT NULL,               -- the lint found a 'warn_special' field
  lint_report           jsonb NOT NULL,                 -- {"blocked":[],"warn_special":["..."],"review_identifier":[]}
  lint_override_by      uuid CONSTRAINT event_form_versions_lint_override_by_fkey REFERENCES users (id),
  lint_override_reason  text CONSTRAINT event_form_versions_lint_override_reason_chk CHECK (char_length(lint_override_reason) BETWEEN 10 AND 500),
  published_by          uuid NOT NULL CONSTRAINT event_form_versions_published_by_fkey REFERENCES users (id),
  published_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT event_form_versions_form_version_uq UNIQUE (form_id, version_no),
  CONSTRAINT event_form_versions_override_chk CHECK ((lint_override_by IS NULL) = (lint_override_reason IS NULL)),
  CONSTRAINT event_form_versions_sensitive_override_chk CHECK (NOT has_sensitive_fields OR lint_override_by IS NOT NULL),
  CONSTRAINT event_form_versions_lint_report_chk CHECK (
    jsonb_typeof(lint_report) = 'object'
    AND coalesce(jsonb_typeof(lint_report -> 'blocked'), '') = 'array'
    AND coalesce(jsonb_typeof(lint_report -> 'warn_special'), '') = 'array'
    AND coalesce(jsonb_typeof(lint_report -> 'review_identifier'), '') = 'array'),
  CONSTRAINT event_form_versions_blocked_chk CHECK (coalesce(lint_report -> 'blocked', '[]'::jsonb) = '[]'::jsonb),
  CONSTRAINT event_form_versions_sensitive_flag_chk CHECK (has_sensitive_fields = (coalesce(lint_report -> 'warn_special', '[]'::jsonb) <> '[]'::jsonb))
);
CREATE TRIGGER event_form_versions_immutable BEFORE UPDATE OR DELETE ON event_form_versions
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER event_form_versions_no_truncate BEFORE TRUNCATE ON event_form_versions
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON event_form_versions TO app_rw;
COMMIT;
