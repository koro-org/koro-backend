-- 000020_create_email_tokens.up.sql (02 §5.3). Single-use e-mail tokens. The
-- token itself is derived (DerivedToken) and never stored: only its SHA-256.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE email_tokens (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id       uuid NOT NULL CONSTRAINT email_tokens_user_id_fkey REFERENCES users (id),
  purpose       text NOT NULL CONSTRAINT email_tokens_purpose_chk CHECK (purpose IN ('account_email','email_change','password_reset','edu_email')),
  target_email  text CONSTRAINT email_tokens_target_email_chk CHECK (char_length(target_email) <= 254),   -- target of email_change / edu_email
  key_id        text NOT NULL,                                    -- derivation key (rotation)
  token_hash    bytea NOT NULL CONSTRAINT email_tokens_token_hash_uq UNIQUE CONSTRAINT email_tokens_token_hash_chk CHECK (octet_length(token_hash) = 32),
  expires_at    timestamptz NOT NULL,                             -- account/edu: 24 hours; reset: 30 minutes
  consumed_at   timestamptz,
  attempts      smallint NOT NULL DEFAULT 0 CONSTRAINT email_tokens_attempts_chk CHECK (attempts BETWEEN 0 AND 10),
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT email_tokens_target_chk CHECK (purpose IN ('account_email','password_reset') OR target_email IS NOT NULL)
);
CREATE INDEX email_tokens_open_idx ON email_tokens (user_id, purpose) WHERE consumed_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON email_tokens TO app_rw;
COMMIT;
