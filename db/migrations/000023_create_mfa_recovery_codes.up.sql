-- 000023_create_mfa_recovery_codes.up.sql (02 §5.6). Single-use recovery
-- codes; only the SHA-256 is stored.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE mfa_recovery_codes (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id     uuid NOT NULL CONSTRAINT mfa_recovery_codes_user_id_fkey REFERENCES users (id),
  code_hash   bytea NOT NULL CONSTRAINT mfa_recovery_codes_code_hash_uq UNIQUE CONSTRAINT mfa_recovery_codes_code_hash_chk CHECK (octet_length(code_hash) = 32),
  used_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mfa_recovery_codes_user_idx ON mfa_recovery_codes (user_id) WHERE used_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON mfa_recovery_codes TO app_rw;
COMMIT;
