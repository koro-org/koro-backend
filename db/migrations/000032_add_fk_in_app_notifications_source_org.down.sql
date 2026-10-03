-- 000032_add_fk_in_app_notifications_source_org.down.sql
BEGIN;
ALTER TABLE in_app_notifications DROP CONSTRAINT IF EXISTS in_app_notifications_source_org_id_fkey;
COMMIT;
