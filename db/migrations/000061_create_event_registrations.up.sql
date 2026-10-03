-- 000061_create_event_registrations.up.sql (02 §11.6). A user's registration
-- for an event; the status machine lives in the service (R-003 §2.5.3), the
-- CHECKs tie each status to its timestamps. The answers are purged on a
-- schedule (answers_purge_at, sensitive_purge_at): the columns are NULLed,
-- the row stays until the account is deleted (hard delete). A user holds at
-- most one live registration per event (event_registrations_active_uq).
-- answers_sensitive_enc is special-category data (envelope encrypted).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE event_registrations (
  id                     uuid PRIMARY KEY DEFAULT uuidv7(),
  event_id               uuid NOT NULL CONSTRAINT event_registrations_event_id_fkey REFERENCES events (id),
  org_id                 uuid NOT NULL CONSTRAINT event_registrations_org_id_fkey REFERENCES organizations (id),
  user_id                uuid NOT NULL CONSTRAINT event_registrations_user_id_fkey REFERENCES users (id),
  status                 text NOT NULL CONSTRAINT event_registrations_status_chk CHECK (status IN ('pending','confirmed','waitlisted','rejected','cancelled',
                                                         'checked_in','no_show','expired','withdrawn')),
  form_version_id        uuid CONSTRAINT event_registrations_form_version_id_fkey REFERENCES event_form_versions (id),   -- NULL for an event without a form
  answers                jsonb NOT NULL DEFAULT '{}',                    -- {"<key>": value}; WITHOUT the warn_special fields
  answers_sensitive_enc  bytea,                                          -- warn_special fields (envelope encrypted)
  consent_snapshot       jsonb NOT NULL DEFAULT '{}',                    -- {"event_sponsor_share":true}; the record of truth is consents
  source                 text NOT NULL CONSTRAINT event_registrations_source_chk CHECK (source IN ('web','mobile','admin')),
  waitlisted_at          timestamptz,
  confirmed_at           timestamptz,
  approved_by            uuid CONSTRAINT event_registrations_approved_by_fkey REFERENCES users (id),
  approved_at            timestamptz,
  decision_note          text CONSTRAINT event_registrations_decision_note_chk CHECK (char_length(decision_note) <= 500),   -- not shown to the student
  cancelled_at           timestamptz,
  cancelled_by_kind      text CONSTRAINT event_registrations_cancelled_by_kind_chk CHECK (cancelled_by_kind IN ('user','org','system')),
  cancel_reason          text CONSTRAINT event_registrations_cancel_reason_chk CHECK (char_length(cancel_reason) <= 300),
  checked_in_at          timestamptz,
  answers_purge_at       timestamptz NOT NULL,      -- event end + 90 days (moves when the end moves)
  sensitive_purge_at     timestamptz,               -- event end + 30 days
  answers_purged_at      timestamptz,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT event_registrations_sensitive_purge_chk CHECK (answers_sensitive_enc IS NULL OR sensitive_purge_at IS NOT NULL),
  CONSTRAINT event_registrations_waitlisted_chk CHECK (status <> 'waitlisted' OR waitlisted_at IS NOT NULL),
  CONSTRAINT event_registrations_confirmed_chk CHECK (status NOT IN ('confirmed','checked_in','no_show') OR confirmed_at IS NOT NULL),
  CONSTRAINT event_registrations_cancelled_chk CHECK (status NOT IN ('cancelled','withdrawn') OR (cancelled_at IS NOT NULL AND cancelled_by_kind IS NOT NULL)),
  CONSTRAINT event_registrations_checked_in_chk CHECK (status <> 'checked_in' OR checked_in_at IS NOT NULL),
  CONSTRAINT event_registrations_purged_chk CHECK (answers_purged_at IS NULL OR (answers = '{}'::jsonb AND answers_sensitive_enc IS NULL))
);
CREATE UNIQUE INDEX event_registrations_active_uq ON event_registrations (event_id, user_id)
  WHERE status NOT IN ('cancelled','rejected','expired','withdrawn');
CREATE INDEX event_registrations_event_idx    ON event_registrations (event_id, status, created_at);
CREATE INDEX event_registrations_user_idx     ON event_registrations (user_id, created_at DESC);
CREATE INDEX event_registrations_waitlist_idx ON event_registrations (event_id, waitlisted_at, id) WHERE status = 'waitlisted';
CREATE INDEX event_registrations_purge_idx    ON event_registrations (answers_purge_at) WHERE answers_purged_at IS NULL;
CREATE TRIGGER event_registrations_touch_updated_at BEFORE UPDATE ON event_registrations
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON event_registrations TO app_rw;
COMMIT;
