-- 000083_create_talent_daily_counters.down.sql
-- DATA LOSS: the daily cap counters.
BEGIN;
DROP TABLE IF EXISTS talent.daily_counters;
COMMIT;
