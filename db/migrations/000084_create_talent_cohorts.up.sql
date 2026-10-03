-- 000084_create_talent_cohorts.up.sql (02 §15.5). Schema only (phase 3). The
-- comparison cohort of a user: university + class year; the university
-- alone below cohort_min; 'global' when even that is too small. app_talent
-- only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.cohorts (
  user_id     uuid PRIMARY KEY CONSTRAINT cohorts_user_id_fkey REFERENCES public.users (id),
  cohort_key  text NOT NULL CONSTRAINT cohorts_cohort_key_chk CHECK (cohort_key ~ '^(uni:[0-9a-f-]{36}(:year:[1-8])?|global)$'),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cohorts_key_idx ON talent.cohorts (cohort_key);
CREATE TRIGGER cohorts_touch_updated_at BEFORE UPDATE ON talent.cohorts
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON talent.cohorts TO app_talent;
COMMIT;
