-- 000032_add_fk_in_app_notifications_source_org.up.sql (02 §7.1). The forward
-- FK of in_app_notifications (000028) now that organizations exists.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE in_app_notifications ADD CONSTRAINT in_app_notifications_source_org_id_fkey
  FOREIGN KEY (source_org_id) REFERENCES organizations (id);
COMMIT;
