-- 000071_create_org_follows.down.sql
-- DATA LOSS: every follow.
BEGIN;
DROP TABLE IF EXISTS org_follows;
COMMIT;
