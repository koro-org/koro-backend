-- 000080_create_talent_signal_defs.down.sql
-- DATA LOSS: the talent signal catalogue.
BEGIN;
DROP TABLE IF EXISTS talent.signal_defs;
COMMIT;
