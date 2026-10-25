-- 000149_files_public_key.down.sql
-- The public copies stay in the public media bucket until operations empty
-- it; nothing refers to them any more.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP INDEX files_unpublished_idx;
ALTER TABLE files DROP COLUMN public_key;
COMMIT;
