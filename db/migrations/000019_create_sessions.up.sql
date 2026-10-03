-- 000019_create_sessions.up.sql (02 §5.2). Opaque server-side sessions; only
-- the SHA-256 of the token is stored. Expired and revoked rows are deleted
-- by the retention sweeper (30 days after the end).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE sessions (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id              uuid NOT NULL CONSTRAINT sessions_user_id_fkey REFERENCES users (id),
  token_hash           bytea NOT NULL CONSTRAINT sessions_token_hash_uq UNIQUE CONSTRAINT sessions_token_hash_chk CHECK (octet_length(token_hash) = 32),  -- SHA-256(opaque token)
  client_kind          text NOT NULL CONSTRAINT sessions_client_kind_chk CHECK (client_kind IN ('web','mobile')),
  created_at           timestamptz NOT NULL DEFAULT now(),
  last_seen_at         timestamptz NOT NULL DEFAULT now(),     -- updated at most once a minute
  idle_expires_at      timestamptz NOT NULL,
  absolute_expires_at  timestamptz NOT NULL,
  mfa_verified_at      timestamptz,
  revoked_at           timestamptz,
  revoke_reason        text CONSTRAINT sessions_revoke_reason_chk CHECK (revoke_reason IN ('logout','password_change','admin','erasure','mfa_reset','suspended','role_change')),
  ip                   inet,
  user_agent           text CONSTRAINT sessions_user_agent_chk CHECK (char_length(user_agent) <= 400),
  device_label         text CONSTRAINT sessions_device_label_chk CHECK (char_length(device_label) <= 100),
  CONSTRAINT sessions_revoked_chk CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
  CONSTRAINT sessions_expiry_chk CHECK (idle_expires_at <= absolute_expires_at)
);
CREATE INDEX sessions_user_active_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry_idx      ON sessions (absolute_expires_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO app_rw;
COMMIT;
