-- 000083_create_talent_daily_counters.up.sql (02 §15.4). Schema only (phase
-- 3). Per user, day (Europe/Istanbul) and signal: the points applied so far,
-- the base of the daily cap (conditional upsert:
-- ON CONFLICT DO UPDATE ... WHERE applied_cp + $x <= $cap). Hard-deleted
-- after 14 days (talent_counters). app_talent only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.daily_counters (
  user_id      uuid NOT NULL CONSTRAINT daily_counters_user_id_fkey REFERENCES public.users (id),
  day          date NOT NULL,
  signal_code  text NOT NULL CONSTRAINT daily_counters_signal_code_fkey REFERENCES talent.signal_defs (code),
  applied_cp   integer NOT NULL,
  CONSTRAINT daily_counters_pkey PRIMARY KEY (user_id, day, signal_code)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON talent.daily_counters TO app_talent;
COMMIT;
