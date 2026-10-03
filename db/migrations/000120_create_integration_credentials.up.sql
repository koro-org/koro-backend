-- 000120_create_integration_credentials.up.sql (02 §22.5). OAuth tokens of
-- the integrations (Paraşüt refresh-token rotation), envelope-encrypted
-- (platform/crypto); static API keys live in the secret manager, not here.
-- The refresh runs in one worker under SELECT … FOR UPDATE. Hard delete.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE integration_credentials (
  provider           text CONSTRAINT integration_credentials_pkey PRIMARY KEY
                       CONSTRAINT integration_credentials_provider_chk CHECK (provider IN ('parasut','iyzico','iys','email')),
  external_account   text,                    -- Paraşüt company_id etc.
  access_token_enc   bytea,
  refresh_token_enc  bytea,
  expires_at         timestamptz,
  rotated_at         timestamptz,
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER integration_credentials_touch_updated_at BEFORE UPDATE ON integration_credentials
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON integration_credentials TO app_rw;
COMMIT;
