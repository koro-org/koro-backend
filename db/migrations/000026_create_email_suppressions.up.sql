-- 000026_create_email_suppressions.up.sql (02 §6.2). Addresses that must not
-- receive mail (hard bounce, complaint), kept as blind indexes only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE email_suppressions (
  email_bidx  bytea PRIMARY KEY CONSTRAINT email_suppressions_email_bidx_chk CHECK (octet_length(email_bidx) = 32),
  reason      text NOT NULL CONSTRAINT email_suppressions_reason_chk CHECK (reason IN ('hard_bounce','complaint','manual','invalid')),
  source      text NOT NULL CONSTRAINT email_suppressions_source_chk CHECK (source IN ('provider_webhook','admin','user_request')),
  created_at  timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON email_suppressions TO app_rw;
COMMIT;
