-- 000079_create_talent_config_versions.up.sql (02 §15.1, §0.3; 06 H-3).
-- Schema only: the talent code is phase 3 and the ledger is not written
-- while talent.default_enabled = false (T-32).
-- The talent schema is reachable by app_talent only. 000001 created it with
-- USAGE for app_talent alone; this first talent table restates the boundary:
-- PUBLIC, app_rw and app_ro hold nothing on the schema (app_rw enters it
-- only through SET LOCAL ROLE app_talent, its membership is NOINHERIT), and
-- every talent table grants to app_talent only.
-- A config version is immutable (02 §15.1 "değiştirilemez"; addition to the
-- 02 §0.5 trigger list): app_talent has SELECT/INSERT and
-- koro_forbid_mutation blocks UPDATE/DELETE/TRUNCATE for everyone.
BEGIN;
SET LOCAL lock_timeout = '5s';
REVOKE ALL ON SCHEMA talent FROM PUBLIC;
REVOKE ALL ON SCHEMA talent FROM app_rw, app_ro;

CREATE TABLE talent.config_versions (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  effective_from  timestamptz NOT NULL CONSTRAINT config_versions_effective_from_uq UNIQUE,
  params          jsonb NOT NULL,
  -- {"shrink_k":5,"cohort_min":30,"lookback_days":540,
  --  "category_caps_cp":{"participation":4000,"intent":1500,"profile":1000,"community":600,"engagement":300},
  --  "bands":[{"band":1,"min_pct_bp":5000,"mult_bp":10500},{"band":2,"min_pct_bp":7500,"mult_bp":11000},
  --           {"band":3,"min_pct_bp":9000,"mult_bp":11500}],
  --  "min_account_age_days_for_likes":7,"min_event_confirmed":5,"min_event_distinct_checkins":3}
  notes           text,
  created_by      uuid NOT NULL CONSTRAINT config_versions_created_by_fkey REFERENCES public.users (id),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER config_versions_immutable BEFORE UPDATE OR DELETE ON talent.config_versions
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER config_versions_no_truncate BEFORE TRUNCATE ON talent.config_versions
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON talent.config_versions TO app_talent;
COMMIT;
