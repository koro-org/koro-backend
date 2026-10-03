-- 000025_create_notification_deliveries.up.sql (02 §6.1). One row per
-- notification delivery; the recipient address is never stored (only its
-- blind index for recipients without an account). dedupe_key makes a
-- retried delivery a no-op.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE notification_deliveries (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id              uuid CONSTRAINT notification_deliveries_user_id_fkey REFERENCES users (id),        -- NULL: recipient without an account (invitation, payee contact)
  recipient_bidx       bytea CONSTRAINT notification_deliveries_recipient_bidx_chk CHECK (octet_length(recipient_bidx) = 32),   -- HMAC(normalised e-mail); the address is not stored
  channel              text NOT NULL CONSTRAINT notification_deliveries_channel_chk CHECK (channel IN ('email','push','in_app')),
  category             text NOT NULL CONSTRAINT notification_deliveries_category_chk CHECK (category IN ('transactional','commercial')),
  template             text NOT NULL CONSTRAINT notification_deliveries_template_chk CHECK (template ~ '^[a-z_]+(\.[a-z_]+)*$'),
  template_version     smallint NOT NULL CONSTRAINT notification_deliveries_template_version_chk CHECK (template_version > 0),
  dedupe_key           text NOT NULL CONSTRAINT notification_deliveries_dedupe_key_uq UNIQUE CONSTRAINT notification_deliveries_dedupe_key_chk CHECK (char_length(dedupe_key) <= 200),
  provider             text,
  provider_message_id  text,
  status               text NOT NULL DEFAULT 'queued' CONSTRAINT notification_deliveries_status_chk CHECK (status IN (
                         'queued','sent','delivered','bounced','complained','failed',
                         'suppressed','skipped_no_consent','skipped_preference')),
  attempts             smallint NOT NULL DEFAULT 0 CONSTRAINT notification_deliveries_attempts_chk CHECK (attempts >= 0),
  last_error           text CONSTRAINT notification_deliveries_last_error_chk CHECK (char_length(last_error) <= 2000),
  sent_at              timestamptz,
  delivered_at         timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT notification_deliveries_recipient_chk CHECK (user_id IS NOT NULL OR recipient_bidx IS NOT NULL),
  CONSTRAINT notification_deliveries_channel_user_chk CHECK (channel = 'email' OR user_id IS NOT NULL)
);
CREATE INDEX notification_deliveries_user_idx ON notification_deliveries (user_id, created_at DESC) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX notification_deliveries_provider_uq ON notification_deliveries (provider, provider_message_id)
  WHERE provider_message_id IS NOT NULL;
CREATE TRIGGER notification_deliveries_touch_updated_at BEFORE UPDATE ON notification_deliveries
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON notification_deliveries TO app_rw;
COMMIT;
