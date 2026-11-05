-- 000177_create_user_phones.up.sql (BE-172; user decision 2026-11-05: SMS/OTP
-- in the pilot; 02 §5 doc patch). A user's verified mobile number and the
-- one-time SMS codes of identity.
-- * user_phones: at most one verified number per account (user_id is the
--   key) and one account per number (user_phones_phone_uq: a number is a
--   sign-in factor and a password reset channel, it must name one account).
--   Only a number its holder proved with a code is stored; a number being
--   verified lives on its sms_codes row only. sms_mfa_enabled_at: the SMS
--   second step of sign-in is on (never for MFA roles: the service refuses
--   it, 03 §6.4; an SMS sign-in never makes a session MFA-verified).
-- * sms_codes: one row per code sent. The code is derived (HMAC under
--   CRYPTO_TOKEN_KEYS, key_id) from the row ID and never stored: code_hash is
--   a keyed MAC of the code (the database alone cannot brute-force the six
--   digits). attempts counts wrong guesses (≤ 5 by the service); the worker
--   re-derives the code, sends it and sets sent_at.
-- * KVKK: both personal (the number). identity's provider exports the
--   number and deletes both tables on erasure; the user removes the number
--   at will (hard delete, no soft delete). Retention sms_code: the daily
--   identity.sms_codes_cleanup deletes a code 7 days after its expiry
--   (operational, never deferred to the legal approval; seeded unapproved,
--   T-35). user_phones lives as long as the account.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE user_phones (
  user_id            uuid PRIMARY KEY CONSTRAINT user_phones_user_id_fkey REFERENCES users (id),
  phone_e164         text NOT NULL CONSTRAINT user_phones_phone_e164_chk CHECK (phone_e164 ~ '^\+905[0-9]{9}$'),
  verified_at        timestamptz NOT NULL,
  sms_mfa_enabled_at timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_phones_phone_uq ON user_phones (phone_e164);
GRANT SELECT, INSERT, UPDATE, DELETE ON user_phones TO app_rw;

CREATE TABLE sms_codes (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id     uuid NOT NULL CONSTRAINT sms_codes_user_id_fkey REFERENCES users (id),
  purpose     text NOT NULL CONSTRAINT sms_codes_purpose_chk CHECK (purpose IN ('phone_verify','login','password_reset')),
  phone_e164  text NOT NULL CONSTRAINT sms_codes_phone_e164_chk CHECK (phone_e164 ~ '^\+905[0-9]{9}$'),
  key_id      text NOT NULL CONSTRAINT sms_codes_key_id_chk CHECK (char_length(key_id) BETWEEN 1 AND 64),
  code_hash   bytea NOT NULL CONSTRAINT sms_codes_code_hash_chk CHECK (octet_length(code_hash) = 32),
  attempts    smallint NOT NULL DEFAULT 0 CONSTRAINT sms_codes_attempts_chk CHECK (attempts BETWEEN 0 AND 10),
  expires_at  timestamptz NOT NULL,
  consumed_at timestamptz,
  sent_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT sms_codes_expiry_chk CHECK (expires_at > created_at)
);
CREATE INDEX sms_codes_open_idx ON sms_codes (user_id, purpose) WHERE consumed_at IS NULL;
CREATE INDEX sms_codes_expires_at_idx ON sms_codes (expires_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON sms_codes TO app_rw;

-- The retention of the codes (02 §24.2 doc patch; a job policy like
-- email_token: identity.sms_codes_cleanup applies the period).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('sms_code', 'identity', 'sms_codes', '7 days', 'expires_at', 'hard_delete', 'Operasyonel (veri minimizasyonu)', true,
   'Tek kullanımlık SMS kodları (telefon numarası, kod özeti) süresi dolduktan 7 gün sonra, hesap silinince hemen silinir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
