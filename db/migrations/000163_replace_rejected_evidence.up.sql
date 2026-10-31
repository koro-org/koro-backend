-- 000163_replace_rejected_evidence.up.sql (BE-154; 00 T-393, Q-37). The
-- evidence file of a financial or legal record (party document, bank account
-- evidence, transfer and refund receipts, payout receipt, delivery evidence,
-- refund request evidence) is frozen with the record. When the file is
-- rejected after its upload (file.rejected: found infected, BE-048, or not
-- processable, BE-059) its object is gone and the record keeps a reference
-- nobody can open (BE-151). The role that could upload the evidence now
-- attaches a new file in its place:
--   * koro_evidence_file_guard(file_column [, guard_column]): BEFORE UPDATE
--     row trigger. Once the guard column of the old row is set (default: the
--     file column itself; 'id' for "always"), the file column changes only
--     away from a rejected file (files.status = 'rejected' or
--     files.scan_status = 'infected') to another file, never to NULL. A clean
--     or pending file stays frozen; nothing else of the frozen sets thaws.
--   * The frozen sets that held a file column (koro_freeze_columns) are
--     recreated without it, under their names, and the file column gets its
--     guard. Columns that were not frozen (legal_party_documents.file_id,
--     incoming_bank_transfers.receipt_file_id, payments.evidence_file_id)
--     get the same guard: once set, they too change only away from a
--     rejected file.
--   * receivable_collections_source_fkey cascades an update of the
--     transfer's receipt: a collection's receipt is its transfer's (BE-130),
--     so replacing the transfer's rejected receipt replaces the collection's
--     in the same statement (its own guard sees the same rejected file).
-- invoices.pdf_file_id is not touched: before the issue it is replaced by an
-- upload as before (invoices_frozen guards it from issued_at on, with
-- pdf_sha256 and the verification; an issued invoice keeps its PDF).
-- The rejected file's row stays attached to the record (its history:
-- purpose, checksum, uploader, status; files:financial retention).
BEGIN;
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION public.koro_evidence_file_guard() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE
  old_row  jsonb := to_jsonb(OLD);
  new_row  jsonb := to_jsonb(NEW);
  file_col text;
  guard    text;
  old_file uuid;
  new_file uuid;
BEGIN
  IF TG_NARGS NOT IN (1, 2) THEN
    RAISE EXCEPTION 'koro_evidence_file_guard on %.%: bad arguments', TG_TABLE_SCHEMA, TG_TABLE_NAME;
  END IF;
  file_col := TG_ARGV[0];
  guard := CASE WHEN TG_NARGS = 2 THEN TG_ARGV[1] ELSE TG_ARGV[0] END;
  IF NOT (old_row ? file_col) OR NOT (old_row ? guard) THEN
    RAISE EXCEPTION 'koro_evidence_file_guard on %.%: no column % or %', TG_TABLE_SCHEMA, TG_TABLE_NAME, file_col, guard;
  END IF;
  IF old_row ->> guard IS NULL OR (old_row -> file_col) IS NOT DISTINCT FROM (new_row -> file_col) THEN
    RETURN NEW;
  END IF;
  old_file := (old_row ->> file_col)::uuid;
  new_file := (new_row ->> file_col)::uuid;
  IF old_file IS NULL OR new_file IS NULL OR NOT EXISTS (
       SELECT 1 FROM public.files f WHERE f.id = old_file AND (f.status = 'rejected' OR f.scan_status = 'infected')) THEN
    RAISE EXCEPTION 'UPDATE of %.% is not allowed: % is frozen once % is set (only a rejected file is replaced)',
      TG_TABLE_SCHEMA, TG_TABLE_NAME, file_col, guard
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $$;

-- parties: the party documents and the bank account evidence.
CREATE TRIGGER legal_party_documents_file_guard BEFORE UPDATE ON legal_party_documents
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('file_id');
DROP TRIGGER payee_bank_accounts_frozen ON payee_bank_accounts;
CREATE TRIGGER payee_bank_accounts_frozen BEFORE UPDATE ON payee_bank_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'payee_id', 'org_id', 'iban', 'holder_name', 'bank_name',
    'submitted_by', 'submitted_by_admin', 'submitted_at');
CREATE TRIGGER payee_bank_accounts_evidence_guard BEFORE UPDATE ON payee_bank_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('evidence_file_id', 'id');

-- payments: the transfer receipt (and its collections), the return receipt,
-- the direct payment's evidence, the refund receipt, the refund request's
-- evidence.
CREATE TRIGGER incoming_bank_transfers_receipt_guard BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('receipt_file_id');
DROP TRIGGER incoming_bank_transfers_returned_frozen ON incoming_bank_transfers;
CREATE TRIGGER incoming_bank_transfers_returned_frozen BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('returned_at', 'status', 'returned_at', 'return_bank_reference');
CREATE TRIGGER incoming_bank_transfers_return_receipt_guard BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('return_receipt_file_id', 'returned_at');
DROP TRIGGER receivable_collections_frozen ON receivable_collections;
CREATE TRIGGER receivable_collections_frozen BEFORE UPDATE ON receivable_collections FOR EACH ROW
  EXECUTE FUNCTION public.koro_freeze_columns('id', 'incoming_transfer_id', 'order_line_id', 'order_id', 'payee_id', 'org_id',
    'collection_account_id', 'amount', 'bank_reference', 'requested_by', 'created_at');
CREATE TRIGGER receivable_collections_receipt_guard BEFORE UPDATE ON receivable_collections
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('receipt_file_id', 'id');
ALTER TABLE receivable_collections DROP CONSTRAINT receivable_collections_source_fkey,
  ADD CONSTRAINT receivable_collections_source_fkey
    FOREIGN KEY (incoming_transfer_id, collection_account_id, amount, bank_reference, receipt_file_id)
    REFERENCES incoming_bank_transfers (id, collection_account_id, amount, bank_reference, receipt_file_id) ON UPDATE CASCADE;
CREATE TRIGGER payments_evidence_guard BEFORE UPDATE ON payments
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('evidence_file_id');
DROP TRIGGER refunds_completed_frozen ON refunds;
CREATE TRIGGER refunds_completed_frozen BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('completed_at', 'status', 'completed_at', 'bank_reference');
CREATE TRIGGER refunds_receipt_guard BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('receipt_file_id', 'completed_at');
DROP TRIGGER refund_requests_frozen ON refund_requests;
CREATE TRIGGER refund_requests_frozen BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_id', 'order_line_id', 'org_id', 'seller_org_id', 'order_no',
    'line_title', 'amount', 'currency', 'reason', 'explanation', 'target_iban', 'requested_by', 'created_at');
CREATE TRIGGER refund_requests_evidence_guard BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('evidence_file_id', 'id');

-- ledger: the payout receipt.
DROP TRIGGER payouts_paid_frozen ON payouts;
CREATE TRIGGER payouts_paid_frozen BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('paid_at', 'status', 'paid_at', 'paid_by', 'bank_reference');
CREATE TRIGGER payouts_receipt_guard BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('receipt_file_id', 'paid_at');

-- orders: the delivery evidence (the trigger keeps its name: 000137's down
-- drops it).
DROP TRIGGER order_lines_evidence_frozen ON order_lines;
CREATE TRIGGER order_lines_evidence_frozen BEFORE UPDATE ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('fulfillment_evidence_file_id');
COMMIT;
