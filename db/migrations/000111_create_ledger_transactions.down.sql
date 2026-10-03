-- 000111_create_ledger_transactions.down.sql
-- DATA LOSS: the whole journal (transaction headers).
BEGIN;
DROP TABLE IF EXISTS ledger_transactions;
COMMIT;
