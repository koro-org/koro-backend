-- 000022_create_mfa_totp.down.sql
-- DATA LOSS: every TOTP enrolment (users must enrol again).
BEGIN;
DROP TABLE IF EXISTS mfa_totp;
COMMIT;
