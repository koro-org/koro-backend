-- 000135_access_grant_resume.down.sql
-- A resumed grant is an active grant: dropping the marker changes no state
-- (only the protection against a late suspension task goes).
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP INDEX profile_access_grants_suspended_idx;
ALTER TABLE profile_access_grants DROP CONSTRAINT profile_access_grants_resumed_chk;
ALTER TABLE profile_access_grants DROP COLUMN resumed_at;
COMMIT;
