-- 000113_create_payouts.down.sql
-- DATA LOSS: every payout with its four-eyes trail and receipts.
BEGIN;
DROP TABLE IF EXISTS payouts;
DROP FUNCTION IF EXISTS public.payouts_check_bank_account();
COMMIT;
