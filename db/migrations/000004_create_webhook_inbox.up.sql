-- 000004_create_webhook_inbox.up.sql (02 §1.2). Deduplicated provider
-- webhooks; raw payloads are nulled after 90 days, rows deleted after 2 years.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE webhook_inbox (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  provider           text NOT NULL CONSTRAINT webhook_inbox_provider_chk CHECK (provider IN ('iyzico','parasut','email','iys')),
  event_key          text NOT NULL CONSTRAINT webhook_inbox_event_key_chk CHECK (char_length(event_key) BETWEEN 1 AND 200), -- provider event id / signature digest
  received_at        timestamptz NOT NULL DEFAULT now(),
  signature_valid    boolean NOT NULL,
  headers            jsonb,                                    -- selected headers; never Authorization
  payload            jsonb,
  payload_sha256     bytea NOT NULL CONSTRAINT webhook_inbox_payload_sha256_chk CHECK (octet_length(payload_sha256) = 32),
  payload_purged_at  timestamptz,
  processed_at       timestamptz,
  attempts           integer NOT NULL DEFAULT 0 CONSTRAINT webhook_inbox_attempts_chk CHECK (attempts >= 0),
  last_error         text CONSTRAINT webhook_inbox_last_error_chk CHECK (char_length(last_error) <= 2000),
  CONSTRAINT webhook_inbox_provider_event_key_uq UNIQUE (provider, event_key),
  CONSTRAINT webhook_inbox_purged_chk CHECK ((payload_purged_at IS NULL) OR (payload IS NULL AND headers IS NULL))
);
CREATE INDEX webhook_inbox_unprocessed_idx ON webhook_inbox (received_at) WHERE processed_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_inbox TO app_rw;
COMMIT;
