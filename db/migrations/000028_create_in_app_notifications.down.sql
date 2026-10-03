-- 000028_create_in_app_notifications.down.sql
-- DATA LOSS: every in-app notification.
BEGIN;
DROP TABLE IF EXISTS in_app_notifications;
COMMIT;
