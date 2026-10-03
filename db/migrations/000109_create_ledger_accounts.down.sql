-- 000109_create_ledger_accounts.down.sql
-- DATA LOSS: the chart of accounts.
BEGIN;
DROP TABLE IF EXISTS ledger_accounts;
COMMIT;
