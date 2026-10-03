-- 000007_create_audit_daily_roots.up.sql (02 §2.2). Signed daily roots of the
-- audit hash chain; immutable for app_rw (SELECT, INSERT only).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE audit_daily_roots (
  day             date PRIMARY KEY,                 -- Europe/Istanbul day
  last_chain_seq  bigint NOT NULL,
  root_hash       bytea NOT NULL CONSTRAINT audit_daily_roots_root_hash_chk CHECK (octet_length(root_hash) = 32),
  signature       bytea NOT NULL,                   -- Ed25519 (OpenBao Transit) signature
  key_id          text NOT NULL,
  external_ref    text NOT NULL,                    -- object key in the separate object-locked store
  created_at      timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT ON audit_daily_roots TO app_rw;
COMMIT;
