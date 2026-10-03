-- 000029_create_notification_preferences.down.sql
-- DATA LOSS: notification preferences (defaults apply again).
BEGIN;
DROP TABLE IF EXISTS notification_preferences;
COMMIT;
