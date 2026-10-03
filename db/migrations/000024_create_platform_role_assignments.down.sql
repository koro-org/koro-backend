-- 000024_create_platform_role_assignments.down.sql
-- DATA LOSS: every platform role assignment (nobody is a platform admin until
-- `koro seed bootstrap-admin` runs again).
BEGIN;
DROP TABLE IF EXISTS platform_role_assignments;
COMMIT;
