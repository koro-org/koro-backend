-- 000136_access_grant_contact_share.down.sql
-- Without the column no grant shares the e-mail (the level 3 view falls back to no contact).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE profile_access_grants DROP CONSTRAINT profile_access_grants_contact_shared_chk;
ALTER TABLE profile_access_grants DROP COLUMN contact_shared_at;
COMMIT;
