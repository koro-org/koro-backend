-- 000135_access_grant_resume.up.sql (BE-088; 02 §9.7). The student may
-- resume a suspended level 3 grant within the suspension window (user
-- decisions Q-03/Q-04, T-331/T-332): resumed_at records that explicit
-- consent act. A suspension task that runs late (a lapse or gap event older
-- than the resume) never suspends a grant resumed after its cut-off. The
-- nightly expiry (students.expire_suspended_grants) reads the open suspended
-- grants by suspended_at.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE profile_access_grants ADD COLUMN resumed_at timestamptz;
ALTER TABLE profile_access_grants ADD CONSTRAINT profile_access_grants_resumed_chk
  CHECK (resumed_at IS NULL OR (resumed_at >= granted_at AND (suspended_at IS NULL OR suspended_at >= resumed_at)));
CREATE INDEX profile_access_grants_suspended_idx ON profile_access_grants (suspended_at)
  WHERE revoked_at IS NULL AND suspended_at IS NOT NULL;
COMMIT;
