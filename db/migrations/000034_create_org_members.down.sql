-- 000034_create_org_members.down.sql
-- DATA LOSS: every membership (and with it every role inside an org).
BEGIN;
DROP TABLE IF EXISTS org_members;
COMMIT;
