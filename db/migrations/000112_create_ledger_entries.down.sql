-- 000112_create_ledger_entries.down.sql
-- DATA LOSS: every ledger entry (all balances).
BEGIN;
DROP TRIGGER IF EXISTS ledger_txn_balanced ON ledger_transactions;
DROP TABLE IF EXISTS ledger_entries;
DROP FUNCTION IF EXISTS public.ledger_check_balanced();
COMMIT;
