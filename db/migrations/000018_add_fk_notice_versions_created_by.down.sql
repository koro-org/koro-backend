-- 000018_add_fk_notice_versions_created_by.down.sql
BEGIN;
ALTER TABLE notice_versions DROP CONSTRAINT IF EXISTS notice_versions_created_by_fkey;
COMMIT;
