-- 000116_create_invoices.up.sql (02 §22.1; 06 H-19). An invoice Koro plans,
-- issues, receives or records: per payment model (02 §18.3 table) the
-- expected invoices are 'planned'; in the MVP (invoice_automation 'manual')
-- finance or the payee uploads the document (manual_upload /
-- seller_uploaded) and another finance admin verifies amount, number and
-- date → 'issued' (or 'mismatch'). The provider mode (Paraşüt) follows
-- queued → provider_pending → issued. Cancelling an issued invoice is a
-- credit note. Never deleted (financial_records; VUK/TTK).
-- 06 H-19 (T-66): the verifier is not the uploader, in the database as well
-- (03 §6.1 had it in the application only): invoices_verifier_chk. Like
-- data_subject_requests_approvers_chk, a verification needs an uploader, so
-- a NULL uploader cannot make `verified_by <> uploaded_by` NULL (= passed).
-- 02 fixes and additions (H-20):
--   * verified_at (new) is set together with verified_by; an uploaded
--     invoice is 'issued' only once verified.
--   * vat_withheld_amount <= vat_amount; the issuer and the recipient differ;
--     a credit note refers to another invoice; the snapshots are JSON objects.
--   * issued_at belongs to the issued states (issued, cancelled after issue).
--   * invoices_frozen: once issued, the parties, number, date, amounts and
--     document never change (a correction is a credit note).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE invoices (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  order_id              uuid CONSTRAINT invoices_order_id_fkey REFERENCES orders (id),
  payout_id             uuid CONSTRAINT invoices_payout_id_fkey REFERENCES payouts (id),
  kind                  text NOT NULL CONSTRAINT invoices_kind_chk CHECK (kind IN ('service_sale','commission','platform_fee','payee_to_koro','credit_note')),
  issuer_party_id       uuid NOT NULL CONSTRAINT invoices_issuer_party_id_fkey REFERENCES legal_parties (id),
  recipient_party_id    uuid NOT NULL CONSTRAINT invoices_recipient_party_id_fkey REFERENCES legal_parties (id),
  issuer_org_id         uuid CONSTRAINT invoices_issuer_org_id_fkey REFERENCES organizations (id),      -- read scope (the payee's org / platform)
  recipient_org_id      uuid CONSTRAINT invoices_recipient_org_id_fkey REFERENCES organizations (id),   -- read scope (buyer company / the payee's org)
  issuer_snapshot       jsonb NOT NULL,
  recipient_snapshot    jsonb NOT NULL,
  issuing_mode          text NOT NULL CONSTRAINT invoices_issuing_mode_chk CHECK (issuing_mode IN ('provider','manual_upload','seller_uploaded','koro_on_behalf')),
  document_type         text NOT NULL DEFAULT 'unknown' CONSTRAINT invoices_document_type_chk CHECK (document_type IN ('e_fatura','e_arsiv','paper','unknown')),
  provider              text NOT NULL DEFAULT 'none' CONSTRAINT invoices_provider_chk CHECK (provider IN ('parasut','nilvera','none')),
  scenario              text CONSTRAINT invoices_scenario_chk CHECK (scenario IN ('basic','commercial')),
  status                text NOT NULL DEFAULT 'planned' CONSTRAINT invoices_status_chk CHECK (status IN ('planned','queued','provider_pending','issued',
                          'failed','mismatch','cancelled')),
  trigger               text NOT NULL CONSTRAINT invoices_trigger_chk CHECK (trigger IN ('on_payment','on_fulfillment','on_payout','manual')),
  reserved_series       text,
  reserved_no           integer,
  provider_contact_id   text,
  provider_invoice_id   text,
  provider_edoc_id      text,
  provider_job_id       text,
  invoice_no            text CONSTRAINT invoices_invoice_no_chk CHECK (char_length(invoice_no) <= 30),
  ettn                  uuid,
  issue_date            date,
  due_date              date,
  currency              char(3) NOT NULL DEFAULT 'TRY' CONSTRAINT invoices_currency_chk CHECK (currency = 'TRY'),
  net_amount            bigint NOT NULL CONSTRAINT invoices_net_amount_chk CHECK (net_amount >= 0),
  vat_amount            bigint NOT NULL CONSTRAINT invoices_vat_amount_chk CHECK (vat_amount >= 0),
  vat_withheld_amount   bigint NOT NULL DEFAULT 0 CONSTRAINT invoices_vat_withheld_amount_chk CHECK (vat_withheld_amount >= 0),
  total_amount          bigint NOT NULL CONSTRAINT invoices_total_amount_chk CHECK (total_amount >= 0),
  payable_amount        bigint NOT NULL CONSTRAINT invoices_payable_amount_chk CHECK (payable_amount >= 0),
  refers_to_invoice_id  uuid CONSTRAINT invoices_refers_to_invoice_id_fkey REFERENCES invoices (id),
  cancellable_until     timestamptz,
  pdf_file_id           uuid CONSTRAINT invoices_pdf_file_id_fkey REFERENCES files (id),
  pdf_sha256            bytea CONSTRAINT invoices_pdf_sha256_chk CHECK (octet_length(pdf_sha256) = 32),
  attempts              integer NOT NULL DEFAULT 0 CONSTRAINT invoices_attempts_chk CHECK (attempts >= 0),
  last_error            text CONSTRAINT invoices_last_error_chk CHECK (char_length(last_error) <= 2000),
  idempotency_key       text NOT NULL CONSTRAINT invoices_idempotency_key_uq UNIQUE,   -- "<kind>:<order_id|payout_id>:<order_line_id|all>:<seq>"
  uploaded_by           uuid CONSTRAINT invoices_uploaded_by_fkey REFERENCES users (id),
  verified_by           uuid CONSTRAINT invoices_verified_by_fkey REFERENCES users (id),
  verified_at           timestamptz,
  issued_at             timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT invoices_source_chk CHECK (order_id IS NOT NULL OR payout_id IS NOT NULL),
  CONSTRAINT invoices_parties_chk CHECK (issuer_party_id <> recipient_party_id),
  CONSTRAINT invoices_snapshots_chk CHECK (jsonb_typeof(issuer_snapshot) = 'object' AND jsonb_typeof(recipient_snapshot) = 'object'),
  CONSTRAINT invoices_credit_note_chk CHECK ((kind = 'credit_note') = (refers_to_invoice_id IS NOT NULL)
                                         AND (refers_to_invoice_id IS NULL OR refers_to_invoice_id <> id)),
  -- amounts (kuruş)
  CONSTRAINT invoices_total_chk CHECK (total_amount = net_amount + vat_amount),
  CONSTRAINT invoices_payable_chk CHECK (payable_amount = total_amount - vat_withheld_amount),
  CONSTRAINT invoices_withheld_chk CHECK (vat_withheld_amount <= vat_amount),
  -- states (06 H-37)
  CONSTRAINT invoices_issued_chk CHECK (status <> 'issued' OR (invoice_no IS NOT NULL AND issue_date IS NOT NULL
                                        AND pdf_file_id IS NOT NULL AND issued_at IS NOT NULL)),
  CONSTRAINT invoices_issued_at_chk CHECK (issued_at IS NULL OR status IN ('issued','cancelled')),
  CONSTRAINT invoices_provider_mode_chk CHECK (issuing_mode <> 'provider' OR provider <> 'none'),
  CONSTRAINT invoices_uploaded_chk CHECK (issuing_mode NOT IN ('manual_upload','seller_uploaded') OR status <> 'issued' OR uploaded_by IS NOT NULL),
  -- four eyes on verification (06 H-19, 03 §6.1)
  CONSTRAINT invoices_verified_pair_chk CHECK ((verified_by IS NULL) = (verified_at IS NULL)),
  CONSTRAINT invoices_verifier_chk CHECK (verified_by IS NULL OR (uploaded_by IS NOT NULL AND verified_by <> uploaded_by)),
  CONSTRAINT invoices_manual_verified_chk CHECK (issuing_mode NOT IN ('manual_upload','seller_uploaded') OR status <> 'issued' OR verified_by IS NOT NULL)
);
CREATE UNIQUE INDEX invoices_no_uq ON invoices (issuer_party_id, invoice_no) WHERE invoice_no IS NOT NULL;
CREATE INDEX invoices_order_idx  ON invoices (order_id) WHERE order_id IS NOT NULL;
CREATE INDEX invoices_payout_idx ON invoices (payout_id) WHERE payout_id IS NOT NULL;
CREATE INDEX invoices_status_idx ON invoices (status, created_at) WHERE status IN ('planned','queued','provider_pending','failed','mismatch');
CREATE TRIGGER invoices_touch_updated_at BEFORE UPDATE ON invoices
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER invoices_frozen BEFORE UPDATE ON invoices
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('issued_at', 'order_id', 'payout_id', 'kind', 'issuer_party_id',
    'recipient_party_id', 'issuer_snapshot', 'recipient_snapshot', 'invoice_no', 'ettn', 'issue_date', 'currency',
    'net_amount', 'vat_amount', 'vat_withheld_amount', 'total_amount', 'payable_amount', 'refers_to_invoice_id',
    'pdf_file_id', 'pdf_sha256', 'uploaded_by', 'verified_by', 'verified_at', 'issued_at');
GRANT SELECT, INSERT, UPDATE ON invoices TO app_rw;
COMMIT;
