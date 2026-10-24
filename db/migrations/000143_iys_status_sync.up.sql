-- 000143_iys_status_sync.up.sql (BE-103; 00 T-312; 02 §4.3, §4.5 doc patch).
-- A refusal the recipient gives İYS directly (or through another channel İYS
-- records) reaches the Koro ledger through consent.iys_status_sync:
--   * consents.channel gains 'iys': the withdrawal row the reconciliation
--     appends (withdrawal only, like 'email_link'; the application refuses
--     a grant through it). The CHECK is replaced NOT VALID and then
--     validated, so the scan of consents holds no write lock; the ledger
--     stays append-only (koro_forbid_mutation is untouched).
--   * iys_status_syncs: one row per İYS channel, the last complete
--     reconciliation (synced_at = the start of that run). The optional
--     send-time hold (CONSENT_IYS_SYNC_MAX_AGE) reads it. No personal data:
--     channel codes and counts only.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE consents DROP CONSTRAINT consents_channel_chk;
ALTER TABLE consents ADD CONSTRAINT consents_channel_chk
  CHECK (channel IN ('web','mobile','email_link','admin','import','iys')) NOT VALID;
ALTER TABLE consents VALIDATE CONSTRAINT consents_channel_chk;
CREATE TABLE iys_status_syncs (
  channel     text PRIMARY KEY CONSTRAINT iys_status_syncs_channel_chk CHECK (channel IN ('email','sms','call')),
  synced_at   timestamptz NOT NULL,
  checked     integer NOT NULL CONSTRAINT iys_status_syncs_checked_chk CHECK (checked >= 0),
  withdrawn   integer NOT NULL CONSTRAINT iys_status_syncs_withdrawn_chk CHECK (withdrawn >= 0 AND withdrawn <= checked),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER iys_status_syncs_touch_updated_at BEFORE UPDATE ON iys_status_syncs
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON iys_status_syncs TO app_rw;
COMMIT;
