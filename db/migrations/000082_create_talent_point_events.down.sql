-- 000082_create_talent_point_events.down.sql
-- DATA LOSS: the whole talent ledger.
BEGIN;
DROP TABLE IF EXISTS talent.point_events;
COMMIT;
