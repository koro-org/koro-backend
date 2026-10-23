-- 000136_access_grant_contact_share.up.sql (BE-089, user decision T-345 2026-10-23; 02 §9.7). Contact sharing is
-- per company: the student shares the e-mail with one company while accepting its contact request
-- (respondMyContactRequest share_contact), and takes it back for that company alone. contact_shared_at is when
-- the student shared it with the grant's company; NULL = not shared. The company sees the e-mail only while the
-- grant is active, contact_shared_at is set and the student's contact_info_share consent is in force (read at
-- every view). A business time like granted_at, never before it.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE profile_access_grants ADD COLUMN contact_shared_at timestamptz;
ALTER TABLE profile_access_grants ADD CONSTRAINT profile_access_grants_contact_shared_chk
  CHECK (contact_shared_at IS NULL OR contact_shared_at >= granted_at);
COMMIT;
