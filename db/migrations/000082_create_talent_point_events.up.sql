-- 000082_create_talent_point_events.up.sql (02 §15.3). Schema only (phase 3).
-- The append-only talent ledger (profiling data). Two layers (02 §0.5):
-- app_talent has SELECT/INSERT only and koro_forbid_mutation blocks
-- UPDATE/DELETE/TRUNCATE for everyone. A reversal is a new row pointing at
-- the original (at most one per row, point_events_reversal_uq). One row per
-- (user, signal, source) (point_events_dedupe_uq) makes the consumers
-- idempotent. app_rw has no privilege at all; nothing is written while
-- talent.default_enabled = false (T-32).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.point_events (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id            uuid NOT NULL CONSTRAINT point_events_user_id_fkey REFERENCES public.users (id),
  signal_code        text NOT NULL CONSTRAINT point_events_signal_code_fkey REFERENCES talent.signal_defs (code),
  source_type        text NOT NULL CONSTRAINT point_events_source_type_chk CHECK (source_type ~ '^[a-z_]{3,40}$'),   -- 'checkin','registration','post','membership','profile_section','day','reversal'
  source_id          uuid NOT NULL,                                          -- on a reversal: the original row id
  occurred_at        timestamptz NOT NULL,                                   -- on a reversal: the original's (decay matches)
  raw_points_cp      integer NOT NULL,
  points_cp          integer NOT NULL,                                       -- applied after the caps
  capped             boolean NOT NULL DEFAULT false,
  cap_reason         text CONSTRAINT point_events_cap_reason_chk CHECK (cap_reason IN ('daily','weekly','per_source')),
  reason_code        text NOT NULL CONSTRAINT point_events_reason_code_chk CHECK (reason_code ~ '^[a-z_]{3,60}$'),
  config_version_id  uuid NOT NULL CONSTRAINT point_events_config_version_id_fkey REFERENCES talent.config_versions (id),
  reversal_of        uuid CONSTRAINT point_events_reversal_of_fkey REFERENCES talent.point_events (id),
  source_event_id    uuid NOT NULL,                                          -- the consumed Envelope.ID (trace)
  created_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT point_events_dedupe_uq UNIQUE (user_id, signal_code, source_type, source_id),
  CONSTRAINT point_events_reversal_chk CHECK ((reversal_of IS NULL) = (source_type <> 'reversal')),
  CONSTRAINT point_events_capped_chk CHECK (capped = (cap_reason IS NOT NULL))
);
CREATE UNIQUE INDEX point_events_reversal_uq ON talent.point_events (reversal_of) WHERE reversal_of IS NOT NULL;
CREATE INDEX point_events_user_idx ON talent.point_events (user_id, occurred_at DESC);
CREATE TRIGGER point_events_immutable BEFORE UPDATE OR DELETE ON talent.point_events
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER point_events_no_truncate BEFORE TRUNCATE ON talent.point_events
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON talent.point_events TO app_talent;
COMMIT;
