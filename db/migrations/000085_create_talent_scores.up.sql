-- 000085_create_talent_scores.up.sql (02 §15.6). Schema only (phase 3). The
-- nightly score of a user (profiling result; rewritten every night). A
-- neutralised user is always band 0. run_id points at talent.score_runs
-- (created by 000086) without a FK, as in 02. app_talent only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.scores (
  user_id            uuid PRIMARY KEY CONSTRAINT scores_user_id_fkey REFERENCES public.users (id),
  raw_cp             integer NOT NULL CONSTRAINT scores_raw_cp_chk CHECK (raw_cp >= 0),
  pct_final          numeric(5,4) NOT NULL CONSTRAINT scores_pct_final_chk CHECK (pct_final BETWEEN 0 AND 1),   -- the only numeric (statistics)
  band               smallint NOT NULL DEFAULT 0 CONSTRAINT scores_band_chk CHECK (band BETWEEN 0 AND 3),
  n_signals          integer NOT NULL CONSTRAINT scores_n_signals_chk CHECK (n_signals >= 0),
  cohort_key         text NOT NULL,
  config_version_id  uuid NOT NULL CONSTRAINT scores_config_version_id_fkey REFERENCES talent.config_versions (id),
  run_id             uuid NOT NULL,                       -- talent.score_runs(id)
  top_contributors   jsonb NOT NULL DEFAULT '[]',         -- top 5 contributions (internal explainability)
  neutralized        boolean NOT NULL DEFAULT false,
  calculated_at      timestamptz NOT NULL,
  CONSTRAINT scores_neutralized_chk CHECK (NOT neutralized OR band = 0)
);
CREATE INDEX scores_band_idx ON talent.scores (band) WHERE band > 0;
GRANT SELECT, INSERT, UPDATE, DELETE ON talent.scores TO app_talent;
COMMIT;
