-- 000080_create_talent_signal_defs.up.sql (02 §15.2). Schema only (phase 3).
-- The talent signal catalogue is data, not code (R-003 §3.3). Retired by
-- enabled = false, never deleted (no DELETE for app_talent). app_talent only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.signal_defs (
  code                     text PRIMARY KEY CONSTRAINT signal_defs_code_chk CHECK (code ~ '^[a-z_]{3,40}$'),
  category                 text NOT NULL CONSTRAINT signal_defs_category_chk CHECK (category IN ('participation','intent','profile','community','engagement')),
  description              text NOT NULL,
  base_points_cp           integer NOT NULL,              -- may be negative (event_no_show)
  daily_cap_cp             integer CONSTRAINT signal_defs_daily_cap_cp_chk CHECK (daily_cap_cp > 0),       -- magnitude of the cap
  weekly_cap_cp            integer CONSTRAINT signal_defs_weekly_cap_cp_chk CHECK (weekly_cap_cp > 0),
  half_life_days           integer CONSTRAINT signal_defs_half_life_days_chk CHECK (half_life_days > 0),   -- NULL = no decay
  excluded_when_sensitive  boolean NOT NULL DEFAULT true,
  enabled                  boolean NOT NULL DEFAULT true,
  updated_by               uuid CONSTRAINT signal_defs_updated_by_fkey REFERENCES public.users (id),
  updated_at               timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER signal_defs_touch_updated_at BEFORE UPDATE ON talent.signal_defs
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON talent.signal_defs TO app_talent;
COMMIT;
