-- 000087_create_talent_neutralizations.down.sql
-- DATA LOSS: every neutralisation record.
BEGIN;
DROP TABLE IF EXISTS talent.neutralizations;
COMMIT;
