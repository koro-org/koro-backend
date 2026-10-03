-- 000027_create_push_tokens.up.sql (02 §6.3). Device push tokens. Push
-- content carries no personal data (only a generic text and a deep link).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE push_tokens (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id       uuid NOT NULL CONSTRAINT push_tokens_user_id_fkey REFERENCES users (id),
  session_id    uuid CONSTRAINT push_tokens_session_id_fkey REFERENCES sessions (id) ON DELETE SET NULL,
  platform      text NOT NULL CONSTRAINT push_tokens_platform_chk CHECK (platform IN ('ios','android','web')),
  token         text NOT NULL CONSTRAINT push_tokens_token_chk CHECK (char_length(token) <= 4096),
  token_sha256  bytea NOT NULL CONSTRAINT push_tokens_token_sha256_uq UNIQUE CONSTRAINT push_tokens_token_sha256_chk CHECK (octet_length(token_sha256) = 32),
  app_version   text CONSTRAINT push_tokens_app_version_chk CHECK (char_length(app_version) <= 40),
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz
);
CREATE INDEX push_tokens_user_idx ON push_tokens (user_id) WHERE revoked_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON push_tokens TO app_rw;
COMMIT;
