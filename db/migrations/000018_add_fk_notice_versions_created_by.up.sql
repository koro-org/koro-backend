-- 000018_add_fk_notice_versions_created_by.up.sql (02 §5.1). The forward FK
-- of notice_versions (000012) now that users exists.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE notice_versions ADD CONSTRAINT notice_versions_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id);
COMMIT;
