-- 000087_create_talent_neutralizations.up.sql (02 §15.8). Schema only (phase
-- 3). A user's talent score is neutral (band 0) while an active row exists:
-- opt-out, a KVKK objection (needs the request id) or an admin decision. At
-- most one active row per user and reason. dsr_id has no FK (schema/layer
-- boundary, 02). Kept 3 years after it is lifted (proof of compliance with
-- the objection). app_talent only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.neutralizations (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id     uuid NOT NULL CONSTRAINT neutralizations_user_id_fkey REFERENCES public.users (id),
  reason      text NOT NULL CONSTRAINT neutralizations_reason_chk CHECK (reason IN ('opt_out','objection','admin')),
  dsr_id      uuid,                                  -- public.data_subject_requests (no FK)
  note        text CONSTRAINT neutralizations_note_chk CHECK (char_length(note) <= 500),
  created_by  uuid CONSTRAINT neutralizations_created_by_fkey REFERENCES public.users (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  lifted_at   timestamptz,
  lifted_by   uuid CONSTRAINT neutralizations_lifted_by_fkey REFERENCES public.users (id),
  CONSTRAINT neutralizations_objection_dsr_chk CHECK (reason <> 'objection' OR dsr_id IS NOT NULL)
);
CREATE UNIQUE INDEX neutralizations_active_uq ON talent.neutralizations (user_id, reason) WHERE lifted_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON talent.neutralizations TO app_talent;
COMMIT;
