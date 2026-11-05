-- 000177_create_user_phones.down.sql
-- DATA LOSS: every verified mobile number (and with it the SMS second step
-- of the accounts that turned it on: they sign in with the password, or
-- TOTP, alone) and every open SMS code. Roll back only together with the
-- code that uses them.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE sms_codes;
DROP TABLE user_phones;
-- The policy goes unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then).
DELETE FROM public.retention_policies WHERE code = 'sms_code'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'sms_code');
COMMIT;
