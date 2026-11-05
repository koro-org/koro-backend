-- 000175_create_web_push_subscriptions.down.sql
-- DATA LOSS: every browser subscription and every web_push preference.
-- Roll back only together with the code that writes them.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE web_push_subscriptions;
DELETE FROM notification_preferences WHERE channel = 'web_push';
ALTER TABLE notification_preferences
  DROP CONSTRAINT notification_preferences_channel_chk,
  ADD CONSTRAINT notification_preferences_channel_chk CHECK (channel IN ('email','push','in_app'));
COMMIT;
