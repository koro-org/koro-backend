-- 000137_add_delivery_evidence.up.sql (BE-093; T-292, user decision
-- 2026-10-23; 02 §3.1, §19.2 doc patch). The seller may attach a delivery
-- evidence file (a photo or a PDF) when it marks an order line delivered:
-- the buyer's admins and finance see it, a dispute is decided with it.
--   * files.purpose gains 'delivery_evidence' (the allow-list and who may
--     upload it are the files module's: images + PDF, 10 MB, org required =
--     the seller org; 03 k30). The CHECK is replaced NOT VALID and then
--     validated, so the scan of files holds no write lock.
--   * order_lines.fulfillment_evidence_file_id → files (NO ACTION like every
--     money-table FK, 02 §0.1): the file is part of the line's financial
--     record (financial_records, 10 years; its destruction follows T-280).
--     orders attaches the file in the marking transaction (files.Attach) after
--     checking that it is a delivery_evidence upload of the line's seller org
--     (06 H-38: never trusted from the request).
--   * order_lines_evidence_chk: only a line that was marked carries evidence
--     (fulfillment_marked_at stays set through confirm, dispute and
--     resolution).
--   * order_lines_evidence_frozen: once set the evidence never changes or
--     goes (the proof of a dispute).
--   * order_lines_evidence_idx: the FK check of a files row's deletion (the
--     orphan purge deletes files rows every day) reads it instead of
--     scanning order_lines. Partial: only lines with evidence. Built in this
--     transaction like 000130's index: before the pilot the table is small.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE files DROP CONSTRAINT files_purpose_chk;
ALTER TABLE files ADD CONSTRAINT files_purpose_chk CHECK (purpose IN (
  'avatar','cv','edu_document','org_logo','event_cover','post_media',
  'club_claim_evidence','party_document','bank_account_evidence',
  'transfer_receipt','payout_receipt','refund_receipt',
  'invoice_pdf','event_export','dsr_export','delivery_evidence')) NOT VALID;
ALTER TABLE files VALIDATE CONSTRAINT files_purpose_chk;
ALTER TABLE order_lines ADD COLUMN fulfillment_evidence_file_id uuid
  CONSTRAINT order_lines_fulfillment_evidence_file_id_fkey REFERENCES files (id);
ALTER TABLE order_lines ADD CONSTRAINT order_lines_evidence_chk
  CHECK (fulfillment_evidence_file_id IS NULL OR fulfillment_marked_at IS NOT NULL);
CREATE INDEX order_lines_evidence_idx ON order_lines (fulfillment_evidence_file_id)
  WHERE fulfillment_evidence_file_id IS NOT NULL;
CREATE TRIGGER order_lines_evidence_frozen BEFORE UPDATE ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('fulfillment_evidence_file_id', 'fulfillment_evidence_file_id');
COMMIT;
