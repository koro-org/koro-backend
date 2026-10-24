-- 000143_iys_status_sync.down.sql
-- DATA LOSS: the reconciliation state (recomputed by the next run). The
-- 'iys' withdrawals stay in the append-only ledger: the old CHECK comes back
-- NOT VALID and is validated only when there is none, so the previous
-- schema is restored exactly on a ledger without İYS withdrawals, and no
-- new one can be written either way.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE iys_status_syncs;
ALTER TABLE consents DROP CONSTRAINT consents_channel_chk;
ALTER TABLE consents ADD CONSTRAINT consents_channel_chk
  CHECK (channel IN ('web','mobile','email_link','admin','import')) NOT VALID;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM consents WHERE channel = 'iys') THEN
    ALTER TABLE consents VALIDATE CONSTRAINT consents_channel_chk;
  END IF;
END $$;
COMMIT;
