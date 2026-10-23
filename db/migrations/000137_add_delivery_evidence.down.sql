-- 000137_add_delivery_evidence.down.sql
-- DATA LOSS: the delivery evidence references of the order lines and every
-- delivery_evidence files row (with nothing referring to them any more; their
-- objects stay in the bucket until the bucket is cleaned).
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER order_lines_evidence_frozen ON order_lines;
ALTER TABLE order_lines DROP COLUMN fulfillment_evidence_file_id;
DELETE FROM files WHERE purpose = 'delivery_evidence';
ALTER TABLE files DROP CONSTRAINT files_purpose_chk;
ALTER TABLE files ADD CONSTRAINT files_purpose_chk CHECK (purpose IN (
  'avatar','cv','edu_document','org_logo','event_cover','post_media',
  'club_claim_evidence','party_document','bank_account_evidence',
  'transfer_receipt','payout_receipt','refund_receipt',
  'invoice_pdf','event_export','dsr_export'));
COMMIT;
