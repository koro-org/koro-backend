-- 000016_create_users.up.sql (02 §5.1). Accounts. A user is never deleted:
-- erasure turns the row into a tombstone (status = 'erased', personal columns
-- NULL) that stays the FK target of financial and audit records (02 §0.5),
-- so app_rw has no DELETE.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE users (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  status             text NOT NULL DEFAULT 'pending_email'
                       CONSTRAINT users_status_chk CHECK (status IN ('pending_email','active','suspended','erased')),
  email              text CONSTRAINT users_email_chk CHECK (char_length(email) <= 254),
  email_normalized   text CONSTRAINT users_email_normalized_chk CHECK (char_length(email_normalized) <= 254),
  email_verified_at  timestamptz,
  password_hash      text,                                    -- argon2id PHC string
  first_name         text COLLATE public.tr CONSTRAINT users_first_name_chk CHECK (char_length(first_name) BETWEEN 1 AND 100),
  last_name          text COLLATE public.tr CONSTRAINT users_last_name_chk CHECK (char_length(last_name) BETWEEN 1 AND 100),
  birth_date         date CONSTRAINT users_birth_date_chk CHECK (birth_date >= DATE '1900-01-01'),
  signup_persona     text NOT NULL CONSTRAINT users_signup_persona_chk CHECK (signup_persona IN ('student','company','university','club','platform')),
  locale             text NOT NULL DEFAULT 'tr-TR' CONSTRAINT users_locale_chk CHECK (locale IN ('tr-TR','en-US')),
  mfa_enabled        boolean NOT NULL DEFAULT false,
  last_login_at      timestamptz,
  suspended_at       timestamptz,
  suspension_reason  text CONSTRAINT users_suspension_reason_chk CHECK (char_length(suspension_reason) <= 500),
  erased_at          timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT users_erased_chk CHECK (status <> 'erased' OR (email IS NULL AND email_normalized IS NULL AND password_hash IS NULL
         AND first_name IS NULL AND last_name IS NULL AND birth_date IS NULL AND erased_at IS NOT NULL)),
  CONSTRAINT users_live_chk CHECK (status = 'erased' OR (email IS NOT NULL AND email_normalized IS NOT NULL
         AND first_name IS NOT NULL AND last_name IS NOT NULL)),
  CONSTRAINT users_suspended_chk CHECK (status <> 'suspended' OR suspended_at IS NOT NULL),
  CONSTRAINT users_student_birth_date_chk CHECK (signup_persona <> 'student' OR status = 'erased' OR birth_date IS NOT NULL)
);
CREATE UNIQUE INDEX users_email_uq ON users (email_normalized) WHERE email_normalized IS NOT NULL;
CREATE INDEX users_name_trgm_idx ON users USING gin (public.koro_norm(first_name || ' ' || last_name) public.gin_trgm_ops)
  WHERE status <> 'erased';                                  -- admin/support search only
CREATE TRIGGER users_touch_updated_at BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON users TO app_rw;
COMMIT;
