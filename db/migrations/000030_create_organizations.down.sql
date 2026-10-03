-- 000030_create_organizations.down.sql
-- DATA LOSS: every organization.
BEGIN;
DROP TABLE IF EXISTS organizations;
COMMIT;
