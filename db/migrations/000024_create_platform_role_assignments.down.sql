-- 000024_create_platform_role_assignments.down.sql
-- DATA LOSS: every platform role assignment (nobody is a platform admin until
-- `koro seed bootstrap-admin` runs again) and every four-eyes account action
-- request (06 H-47).
BEGIN;
DROP TABLE IF EXISTS privileged_account_actions;
DROP TABLE IF EXISTS platform_role_assignments;
COMMIT;
