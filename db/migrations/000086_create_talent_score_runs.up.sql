-- 000086_create_talent_score_runs.up.sql (02 §15.7). Schema only (phase 3).
-- One row per nightly scoring run. Hard-deleted after 1 year (talent_runs).
-- app_talent only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.score_runs (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  config_version_id  uuid NOT NULL CONSTRAINT score_runs_config_version_id_fkey REFERENCES talent.config_versions (id),
  started_at         timestamptz NOT NULL DEFAULT now(),
  finished_at        timestamptz,
  users_processed    integer NOT NULL DEFAULT 0,
  status             text NOT NULL DEFAULT 'running' CONSTRAINT score_runs_status_chk CHECK (status IN ('running','succeeded','failed')),
  error              text CONSTRAINT score_runs_error_chk CHECK (char_length(error) <= 2000)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON talent.score_runs TO app_talent;
COMMIT;
