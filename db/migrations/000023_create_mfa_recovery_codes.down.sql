-- 000023_create_mfa_recovery_codes.down.sql
-- DATA LOSS: unused MFA recovery codes.
BEGIN;
DROP TABLE IF EXISTS mfa_recovery_codes;
COMMIT;
