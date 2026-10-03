-- 000051_create_profile_access_grants.down.sql
-- DATA LOSS: every level 3 access grant (companies lose access to profiles).
BEGIN;
DROP TABLE IF EXISTS profile_access_grants;
COMMIT;
