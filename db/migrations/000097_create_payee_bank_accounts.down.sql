-- 000097_create_payee_bank_accounts.down.sql
-- DATA LOSS: every payee bank account (IBANs) with its four-eyes trail.
BEGIN;
DROP TABLE IF EXISTS payee_bank_accounts;
DROP FUNCTION IF EXISTS public.koro_freeze_columns();
DROP FUNCTION IF EXISTS public.koro_initial_status();
COMMIT;
