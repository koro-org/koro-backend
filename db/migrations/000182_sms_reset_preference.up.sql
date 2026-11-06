-- 000182_sms_reset_preference.up.sql (BE-186; user decision Q-51). The password
-- reset by SMS becomes a choice of the account's holder, and a change of the
-- phone factor may ask for a code of the current number.
-- * user_phones.sms_reset_enabled_at: the holder turned the reset by SMS on at
--   that time; NULL (the default, every existing row): off, the reset goes by
--   e-mail. user_phones_sms_reset_sms_mfa_chk: the number is either the reset
--   channel or the SMS second step of an account, never both. The service
--   refuses the choice to role holders and starts honouring it a wait after
--   it was made (identity.SMSResetWait).
-- * sms_codes.purpose phone_change: the code to the current number that
--   removing or replacing the phone factor asks for while the SMS second step
--   is on (identity sendPhoneChangeCode).
-- * KVKK: the date is the subject's own data on an existing personal table:
--   exported and deleted with the number (identity's provider).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE user_phones
  ADD COLUMN sms_reset_enabled_at timestamptz,
  ADD CONSTRAINT user_phones_sms_reset_sms_mfa_chk CHECK (sms_reset_enabled_at IS NULL OR sms_mfa_enabled_at IS NULL);
ALTER TABLE sms_codes
  DROP CONSTRAINT sms_codes_purpose_chk,
  ADD CONSTRAINT sms_codes_purpose_chk CHECK (purpose IN ('phone_verify','login','password_reset','phone_change'));
COMMIT;
