-- 000084_create_talent_cohorts.down.sql
-- DATA LOSS: the cohort of every user.
BEGIN;
DROP TABLE IF EXISTS talent.cohorts;
COMMIT;
