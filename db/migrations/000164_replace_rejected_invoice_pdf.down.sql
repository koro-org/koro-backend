-- 000164_replace_rejected_invoice_pdf.down.sql: an issued invoice's PDF and
-- verification are frozen again, as before BE-154b. An issued invoice whose
-- replaced PDF still waits for its verification fails
-- invoices_manual_verified_chk: verify it first.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER invoices_pdf_reverify ON invoices;
DROP FUNCTION public.koro_invoice_pdf_reverify();
DROP TRIGGER invoices_pdf_guard ON invoices;
DROP TRIGGER invoices_frozen ON invoices;
CREATE TRIGGER invoices_frozen BEFORE UPDATE ON invoices
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('issued_at', 'order_id', 'payout_id', 'kind', 'issuer_party_id',
    'recipient_party_id', 'issuer_snapshot', 'recipient_snapshot', 'invoice_no', 'ettn', 'issue_date', 'currency',
    'net_amount', 'vat_amount', 'vat_withheld_amount', 'total_amount', 'payable_amount', 'refers_to_invoice_id',
    'pdf_file_id', 'pdf_sha256', 'uploaded_by', 'verified_by', 'verified_at', 'issued_at');
ALTER TABLE invoices DROP CONSTRAINT invoices_manual_verified_chk,
  ADD CONSTRAINT invoices_manual_verified_chk CHECK (issuing_mode NOT IN ('manual_upload','seller_uploaded') OR status <> 'issued'
    OR verified_by IS NOT NULL);
ALTER TABLE invoices DROP CONSTRAINT invoices_pdf_replaced_chk;
ALTER TABLE invoices DROP COLUMN pdf_replaced_at;
COMMIT;
