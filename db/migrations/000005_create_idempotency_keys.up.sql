-- 000005_create_idempotency_keys.up.sql (02 §1.3; 06 H-5). Written by the
-- service in the business transaction; kept for 24 hours.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE idempotency_keys (
  scope             text NOT NULL,          -- "<actor_user_id>:<METHOD> <path template>"
  key               text NOT NULL CONSTRAINT idempotency_keys_key_chk CHECK (char_length(key) BETWEEN 8 AND 255),
  request_hash      bytea NOT NULL CONSTRAINT idempotency_keys_request_hash_chk CHECK (octet_length(request_hash) = 32), -- canonical body SHA-256
  status            text NOT NULL DEFAULT 'in_progress' CONSTRAINT idempotency_keys_status_chk CHECK (status IN ('in_progress','completed')),
  response_status   smallint,
  response_headers  jsonb,
  response_body     jsonb,
  locked_until      timestamptz NOT NULL,   -- in_progress lock (30 s)
  created_at        timestamptz NOT NULL DEFAULT now(),
  expires_at        timestamptz NOT NULL,   -- created_at + 24 hours
  PRIMARY KEY (scope, key),
  CONSTRAINT idempotency_keys_response_chk CHECK (status = 'in_progress' OR response_status IS NOT NULL)
);
CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO app_rw;
COMMIT;
