-- 000156_create_refund_requests.down.sql
-- DATA LOSS: every refund request (the refunds they opened stay) and every
-- refund_request_evidence files row (nothing refers to them any more; their
-- objects stay in the bucket until the bucket is cleaned).
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE refund_requests;
ALTER TABLE order_lines DROP CONSTRAINT order_lines_id_seller_uq;
DELETE FROM files WHERE purpose = 'refund_request_evidence';
ALTER TABLE files DROP CONSTRAINT files_purpose_chk;
ALTER TABLE files ADD CONSTRAINT files_purpose_chk CHECK (purpose IN (
  'avatar','cv','edu_document','org_logo','event_cover','post_media',
  'club_claim_evidence','party_document','bank_account_evidence',
  'transfer_receipt','payout_receipt','refund_receipt',
  'invoice_pdf','event_export','dsr_export','delivery_evidence'));
COMMIT;
