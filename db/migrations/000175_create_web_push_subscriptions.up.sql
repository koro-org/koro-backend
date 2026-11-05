-- 000175_create_web_push_subscriptions.up.sql (BE-173; user decision of
-- 2026-11-05: PWA + web push now). Browsers subscribed over the Push API
-- with Koro's VAPID key (RFC 8030, 8291, 8292) and the "web_push" switch of
-- the notification preferences.
--   * web_push_subscriptions: one row per browser subscription
--     (endpoint_sha256 unique: the same browser registering again, also
--     for another account, moves the row). endpoint is a capability URL of
--     a known push service (FCM, Mozilla, Apple, Microsoft; checked by the
--     service, the SSRF guard), p256dh/auth encrypt the payload; none of
--     them leaves the backend except towards the push service. Bound to the
--     session that registered it (SET NULL when the session row goes, as
--     push_tokens). At most 10 per user (the service drops the least
--     recently seen). Personal data: kept while the account lives (the
--     notify KVKK provider exports and erases it) and deleted at once when
--     the push service answers 404/410 (gone) or the user removes it. No
--     retention policy: no last-use clock to purge on (the push service
--     reports dead subscriptions on the next send).
--   * notification_preferences_channel_chk takes 'web_push': the push
--     channel reaches mobile devices (push) and browsers (web_push), each
--     switched per category.
BEGIN;
SET LOCAL lock_timeout = '5s';

CREATE TABLE web_push_subscriptions (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id          uuid NOT NULL CONSTRAINT web_push_subscriptions_user_id_fkey REFERENCES users (id),
  session_id       uuid CONSTRAINT web_push_subscriptions_session_id_fkey REFERENCES sessions (id) ON DELETE SET NULL,
  endpoint         text NOT NULL CONSTRAINT web_push_subscriptions_endpoint_chk
                     CHECK (char_length(endpoint) BETWEEN 9 AND 2048 AND left(endpoint, 8) = 'https://'),
  endpoint_sha256  bytea NOT NULL CONSTRAINT web_push_subscriptions_endpoint_sha256_uq UNIQUE
                     CONSTRAINT web_push_subscriptions_endpoint_sha256_chk CHECK (octet_length(endpoint_sha256) = 32),
  p256dh           bytea NOT NULL CONSTRAINT web_push_subscriptions_p256dh_chk CHECK (octet_length(p256dh) = 65),
  auth             bytea NOT NULL CONSTRAINT web_push_subscriptions_auth_chk CHECK (octet_length(auth) = 16),
  created_at       timestamptz NOT NULL DEFAULT now(),
  last_seen_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX web_push_subscriptions_user_idx ON web_push_subscriptions (user_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON web_push_subscriptions TO app_rw;

ALTER TABLE notification_preferences
  DROP CONSTRAINT notification_preferences_channel_chk,
  ADD CONSTRAINT notification_preferences_channel_chk CHECK (channel IN ('email','push','in_app','web_push'));
COMMIT;
