-- 000159_organizations_rejection_reason.down.sql (BE-144). The rejection
-- reasons are dropped; the audit records of closeOrg keep them.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE organizations DROP COLUMN rejection_reason;
COMMIT;
