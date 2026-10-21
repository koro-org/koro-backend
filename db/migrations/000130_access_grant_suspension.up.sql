-- 000130_access_grant_suspension.up.sql (BE-077; 02 §9.7). A level 3 grant
-- is suspended when the student's verification lapses (KVKK md. 11, user
-- decision 2026-10-21): a suspended grant gives the company nothing and
-- never reopens by itself (the student revokes it, or accepts a new contact
-- request of the company after verifying again). Open = revoked_at IS NULL;
-- active = open and suspended_at IS NULL. The company list index covers the
-- active grants only.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE profile_access_grants ADD COLUMN suspended_at timestamptz;
ALTER TABLE profile_access_grants ADD CONSTRAINT profile_access_grants_suspended_chk
  CHECK (suspended_at IS NULL OR suspended_at >= granted_at);
DROP INDEX profile_access_grants_company_idx;
CREATE INDEX profile_access_grants_company_idx ON profile_access_grants (company_org_id, granted_at DESC)
  WHERE revoked_at IS NULL AND suspended_at IS NULL;
COMMIT;
