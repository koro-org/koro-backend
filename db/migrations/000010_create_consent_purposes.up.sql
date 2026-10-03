-- 000010_create_consent_purposes.up.sql (02 §4.1). Reference table of consent
-- purposes (what a user can consent to); seeded by 000011. Rows are never
-- deleted (active = false retires one), so app_rw has no DELETE.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE consent_purposes (
  code            text PRIMARY KEY CONSTRAINT consent_purposes_code_chk CHECK (code ~ '^[a-z][a-z0-9_]{2,40}$'),
  description_tr  text NOT NULL,
  legal_basis     text NOT NULL CONSTRAINT consent_purposes_legal_basis_chk CHECK (legal_basis IN ('explicit_consent','contract','legal_obligation','legitimate_interest')),
  requires_iys    boolean NOT NULL DEFAULT false,
  iys_channel     text CONSTRAINT consent_purposes_iys_channel_chk CHECK (iys_channel IN ('email','sms','call')),
  scope_type      text NOT NULL DEFAULT 'none' CONSTRAINT consent_purposes_scope_type_chk CHECK (scope_type IN ('none','registration')),
  active          boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT consent_purposes_iys_chk CHECK (requires_iys = (iys_channel IS NOT NULL))
);
GRANT SELECT, INSERT, UPDATE ON consent_purposes TO app_rw;
COMMIT;
