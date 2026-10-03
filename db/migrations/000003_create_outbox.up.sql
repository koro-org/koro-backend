-- 000003_create_outbox.up.sql (02 §1.1). Transactional outbox: one row = one
-- domain event; written in the business transaction, published by the relay.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE outbox (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),           -- = Envelope.ID
  topic           text NOT NULL CONSTRAINT outbox_topic_chk CHECK (topic ~ '^[a-z_]+\.[a-z_]+$'),
  version         smallint NOT NULL DEFAULT 1 CONSTRAINT outbox_version_chk CHECK (version > 0),
  aggregate_type  text NOT NULL,
  aggregate_id    uuid NOT NULL,
  payload         jsonb NOT NULL,                              -- no personal data (01 §0-6)
  actor_user_id   uuid,                                        -- no FK: infrastructure table
  request_id      text,
  occurred_at     timestamptz NOT NULL DEFAULT now(),
  available_at    timestamptz NOT NULL DEFAULT now(),          -- moved forward on retry
  published_at    timestamptz,
  attempts        integer NOT NULL DEFAULT 0 CONSTRAINT outbox_attempts_chk CHECK (attempts >= 0),
  last_error      text CONSTRAINT outbox_last_error_chk CHECK (char_length(last_error) <= 2000)
);
CREATE INDEX outbox_pending_idx   ON outbox (available_at, id) WHERE published_at IS NULL;
CREATE INDEX outbox_published_idx ON outbox (published_at)     WHERE published_at IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON outbox TO app_rw;
COMMIT;
