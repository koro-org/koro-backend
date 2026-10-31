-- 000163_replace_rejected_evidence.down.sql: the evidence files are frozen
-- with their records again, as before BE-154. Replaced files stay replaced
-- (no data changes); the old rows stay attached to their records.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER order_lines_evidence_frozen ON order_lines;
CREATE TRIGGER order_lines_evidence_frozen BEFORE UPDATE ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('fulfillment_evidence_file_id', 'fulfillment_evidence_file_id');

DROP TRIGGER payouts_receipt_guard ON payouts;
DROP TRIGGER payouts_paid_frozen ON payouts;
CREATE TRIGGER payouts_paid_frozen BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('paid_at', 'status', 'paid_at', 'paid_by', 'bank_reference', 'receipt_file_id');

DROP TRIGGER refund_requests_evidence_guard ON refund_requests;
DROP TRIGGER refund_requests_frozen ON refund_requests;
CREATE TRIGGER refund_requests_frozen BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_id', 'order_line_id', 'org_id', 'seller_org_id', 'order_no',
    'line_title', 'amount', 'currency', 'reason', 'explanation', 'target_iban', 'evidence_file_id', 'requested_by', 'created_at');
DROP TRIGGER refunds_receipt_guard ON refunds;
DROP TRIGGER refunds_completed_frozen ON refunds;
CREATE TRIGGER refunds_completed_frozen BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('completed_at', 'status', 'completed_at', 'bank_reference', 'receipt_file_id');
DROP TRIGGER payments_evidence_guard ON payments;
ALTER TABLE receivable_collections DROP CONSTRAINT receivable_collections_source_fkey,
  ADD CONSTRAINT receivable_collections_source_fkey
    FOREIGN KEY (incoming_transfer_id, collection_account_id, amount, bank_reference, receipt_file_id)
    REFERENCES incoming_bank_transfers (id, collection_account_id, amount, bank_reference, receipt_file_id);
DROP TRIGGER receivable_collections_receipt_guard ON receivable_collections;
DROP TRIGGER receivable_collections_frozen ON receivable_collections;
CREATE TRIGGER receivable_collections_frozen BEFORE UPDATE ON receivable_collections FOR EACH ROW
  EXECUTE FUNCTION public.koro_freeze_columns('id', 'incoming_transfer_id', 'order_line_id', 'order_id', 'payee_id', 'org_id',
    'collection_account_id', 'amount', 'bank_reference', 'receipt_file_id', 'requested_by', 'created_at');
DROP TRIGGER incoming_bank_transfers_return_receipt_guard ON incoming_bank_transfers;
DROP TRIGGER incoming_bank_transfers_returned_frozen ON incoming_bank_transfers;
CREATE TRIGGER incoming_bank_transfers_returned_frozen BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('returned_at', 'status', 'returned_at', 'return_bank_reference', 'return_receipt_file_id');
DROP TRIGGER incoming_bank_transfers_receipt_guard ON incoming_bank_transfers;

DROP TRIGGER payee_bank_accounts_evidence_guard ON payee_bank_accounts;
DROP TRIGGER payee_bank_accounts_frozen ON payee_bank_accounts;
CREATE TRIGGER payee_bank_accounts_frozen BEFORE UPDATE ON payee_bank_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'payee_id', 'org_id', 'iban', 'holder_name', 'bank_name',
    'evidence_file_id', 'submitted_by', 'submitted_by_admin', 'submitted_at');
DROP TRIGGER legal_party_documents_file_guard ON legal_party_documents;
DROP FUNCTION public.koro_evidence_file_guard();
COMMIT;
