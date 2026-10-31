-- 000164_replace_rejected_invoice_pdf.up.sql (BE-154b; 00 T-397, T-393,
-- Q-37). The PDF of an issued uploaded invoice that the scan rejects after
-- its upload (file.rejected: infected, BE-048; not processable, BE-059)
-- is uploaded again; the invoice stays issued and everything that makes it
-- the invoice (parties, number, ETTN, date, amounts) stays frozen. The new
-- PDF is not the verified one: its verification is reset and another
-- person than its uploader verifies it (06 H-19, invoices_verifier_chk).
--   * pdf_replaced_at: when an issued invoice's rejected PDF was replaced
--     (never before the issue; it only grows).
--   * invoices_frozen is recreated under its name without pdf_file_id,
--     pdf_sha256, uploaded_by, verified_by and verified_at; they are
--     guarded instead:
--       - invoices_pdf_guard (000163 koro_evidence_file_guard): once issued,
--         pdf_file_id changes only away from a rejected file, never to NULL;
--       - invoices_pdf_reverify: once issued, a new PDF comes with its
--         uploader, its checksum, a new pdf_replaced_at and no
--         verification; otherwise the checksum, the uploader and
--         pdf_replaced_at stay, and the verification is written only to
--         complete a pending re-verification (it was reset by a
--         replacement), once.
--   * invoices_manual_verified_chk: an issued uploaded invoice is verified
--     or its replaced PDF waits for the verification.
-- Before the issue the PDF changes by an upload as before.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE invoices ADD COLUMN pdf_replaced_at timestamptz;
ALTER TABLE invoices ADD CONSTRAINT invoices_pdf_replaced_chk
  CHECK (pdf_replaced_at IS NULL OR (issued_at IS NOT NULL AND pdf_replaced_at >= issued_at));
ALTER TABLE invoices DROP CONSTRAINT invoices_manual_verified_chk,
  ADD CONSTRAINT invoices_manual_verified_chk CHECK (issuing_mode NOT IN ('manual_upload','seller_uploaded') OR status <> 'issued'
    OR verified_by IS NOT NULL OR pdf_replaced_at IS NOT NULL);

DROP TRIGGER invoices_frozen ON invoices;
CREATE TRIGGER invoices_frozen BEFORE UPDATE ON invoices
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('issued_at', 'order_id', 'payout_id', 'kind', 'issuer_party_id',
    'recipient_party_id', 'issuer_snapshot', 'recipient_snapshot', 'invoice_no', 'ettn', 'issue_date', 'currency',
    'net_amount', 'vat_amount', 'vat_withheld_amount', 'total_amount', 'payable_amount', 'refers_to_invoice_id', 'issued_at');
CREATE TRIGGER invoices_pdf_guard BEFORE UPDATE ON invoices
  FOR EACH ROW EXECUTE FUNCTION public.koro_evidence_file_guard('pdf_file_id', 'issued_at');

CREATE FUNCTION public.koro_invoice_pdf_reverify() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
  IF TG_OP = 'INSERT' OR OLD.issued_at IS NULL THEN
    IF NEW.pdf_replaced_at IS NOT NULL THEN
      RAISE EXCEPTION 'invoices.pdf_replaced_at is set only by the replacement of an issued invoice''s PDF'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'invoices_pdf_replaced_chk';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.pdf_file_id IS DISTINCT FROM OLD.pdf_file_id THEN
    -- A replacement (invoices_pdf_guard: of a rejected PDF): its uploader,
    -- a later pdf_replaced_at and no verification.
    IF NEW.uploaded_by IS NULL OR NEW.verified_by IS NOT NULL OR NEW.verified_at IS NOT NULL OR NEW.pdf_replaced_at IS NULL
       OR NEW.pdf_replaced_at < OLD.pdf_replaced_at THEN
      RAISE EXCEPTION 'UPDATE of invoices is not allowed: a replaced PDF of an issued invoice is uploaded by someone and verified again'
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.pdf_sha256 IS DISTINCT FROM OLD.pdf_sha256 OR NEW.uploaded_by IS DISTINCT FROM OLD.uploaded_by
     OR NEW.pdf_replaced_at IS DISTINCT FROM OLD.pdf_replaced_at
     OR (NEW.verified_by IS DISTINCT FROM OLD.verified_by OR NEW.verified_at IS DISTINCT FROM OLD.verified_at)
        AND (OLD.verified_by IS NOT NULL OR OLD.pdf_replaced_at IS NULL) THEN
    RAISE EXCEPTION 'UPDATE of invoices is not allowed: the document of an issued invoice is frozen (only a pending re-verification completes)'
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER invoices_pdf_reverify BEFORE INSERT OR UPDATE ON invoices
  FOR EACH ROW EXECUTE FUNCTION public.koro_invoice_pdf_reverify();
COMMIT;
