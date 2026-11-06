-- 000182_sms_reset_preference.down.sql (BE-186).
-- DATA LOSS: every holder's choice of the password reset by SMS and every open
-- phone_change code. Roll back only together with the code that uses them.
BEGIN;
SET LOCAL lock_timeout = '5s';
DELETE FROM sms_codes WHERE purpose = 'phone_change';
ALTER TABLE sms_codes
  DROP CONSTRAINT sms_codes_purpose_chk,
  ADD CONSTRAINT sms_codes_purpose_chk CHECK (purpose IN ('phone_verify','login','password_reset'));
ALTER TABLE user_phones
  DROP CONSTRAINT user_phones_sms_reset_sms_mfa_chk,
  DROP COLUMN sms_reset_enabled_at;
COMMIT;
