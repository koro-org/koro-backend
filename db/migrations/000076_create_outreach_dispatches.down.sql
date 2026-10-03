-- 000076_create_outreach_dispatches.down.sql
-- DATA LOSS: every dispatch record.
BEGIN;
DROP TABLE IF EXISTS outreach_dispatches;
COMMIT;
