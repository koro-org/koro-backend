-- 000103_create_collection_accounts.down.sql
-- DATA LOSS: the Koro collection accounts with their approvals.
BEGIN;
DROP TABLE IF EXISTS collection_accounts;
COMMIT;
