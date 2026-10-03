-- 000029_create_notification_preferences.up.sql (02 §6.5). Per category and
-- channel opt-outs for non-commercial notifications (commercial messages are
-- governed by consents, not by this table). A missing row means the default
-- of the category (in code).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE notification_preferences (
  user_id     uuid NOT NULL CONSTRAINT notification_preferences_user_id_fkey REFERENCES users (id),
  category    text NOT NULL CONSTRAINT notification_preferences_category_chk CHECK (category IN (
                'security','registration_updates','event_updates','contact_requests',
                'feed_follows','orders_payments','moderation')),
  channel     text NOT NULL CONSTRAINT notification_preferences_channel_chk CHECK (channel IN ('email','push','in_app')),
  enabled     boolean NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, category, channel),
  CONSTRAINT notification_preferences_security_chk CHECK (category <> 'security' OR enabled)        -- security notifications cannot be switched off
);
CREATE TRIGGER notification_preferences_touch_updated_at BEFORE UPDATE ON notification_preferences
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON notification_preferences TO app_rw;
COMMIT;
