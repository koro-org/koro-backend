-- 000105_create_incoming_bank_transfers.down.sql
-- DATA LOSS: every recorded incoming transfer with its match and return trail.
BEGIN;
DROP TABLE IF EXISTS incoming_bank_transfers;
COMMIT;
