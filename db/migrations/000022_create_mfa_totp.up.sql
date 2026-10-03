-- 000022_create_mfa_totp.up.sql (02 §5.5). TOTP secret, envelope encrypted
-- by the application.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE mfa_totp (
  user_id         uuid PRIMARY KEY CONSTRAINT mfa_totp_user_id_fkey REFERENCES users (id),
  secret_enc      bytea NOT NULL,                 -- envelope encrypted
  confirmed_at    timestamptz,
  last_used_step  bigint,                         -- replay guard
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER mfa_totp_touch_updated_at BEFORE UPDATE ON mfa_totp
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON mfa_totp TO app_rw;
COMMIT;
