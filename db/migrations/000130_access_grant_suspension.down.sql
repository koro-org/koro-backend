-- 000130_access_grant_suspension.down.sql
-- A suspended grant must never become active again: the rollback ends every
-- open suspended grant (revoked by the system at its suspension) before the
-- column goes.
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE profile_access_grants SET revoked_at = suspended_at, revoked_by_kind = 'system'
 WHERE suspended_at IS NOT NULL AND revoked_at IS NULL;
DROP INDEX profile_access_grants_company_idx;
CREATE INDEX profile_access_grants_company_idx ON profile_access_grants (company_org_id, granted_at DESC) WHERE revoked_at IS NULL;
ALTER TABLE profile_access_grants DROP CONSTRAINT profile_access_grants_suspended_chk;
ALTER TABLE profile_access_grants DROP COLUMN suspended_at;
COMMIT;
