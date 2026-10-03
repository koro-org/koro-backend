-- 000094_create_legal_parties.up.sql (02 §17.1). The legal party of money and
-- invoices (ADR-0014): Koro itself (kind 'platform', a single row), a buyer
-- company, or a payee (the foundation / university / club entity that is paid
-- on behalf of a seller org). One foundation may be the payee of many clubs
-- (one payees row per club). An individual (şahıs) party carries the national
-- ID only as an envelope-encrypted value plus a blind index (02 §0.4). Rows
-- tied to financial records are never deleted (financial_records, 10 years):
-- app_rw has no DELETE; the end-of-retention purge is a separate function.
-- 02 fixes (H-20):
--   * 06 H-37: the CHECKs `(legal_type = 'individual') = (tckn_enc IS NOT NULL
--     AND tckn_bidx IS NOT NULL)` and `(status = 'verified') = (verified_at IS
--     NOT NULL AND verified_by IS NOT NULL)` let a row outside the state carry
--     half of the state's columns; each column is now tied to the state.
--   * legal_parties_id_org_uq (06 H-38): the target of orders
--     (buyer_party_id, org_id), so an order is billed to a party of the buyer.
--   * legal_parties_address_chk: address is a JSON object.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE legal_parties (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  kind                  text NOT NULL CONSTRAINT legal_parties_kind_chk CHECK (kind IN ('platform','buyer_company','payee')),
  legal_type            text NOT NULL CONSTRAINT legal_parties_legal_type_chk CHECK (legal_type IN ('company','university','foundation','association',
                          'sole_prop','individual','public_body','other')),
  org_id                uuid CONSTRAINT legal_parties_org_id_fkey REFERENCES organizations (id),   -- company / university org (NULL for a foundation)
  title                 text COLLATE "tr" NOT NULL CONSTRAINT legal_parties_title_chk CHECK (char_length(title) BETWEEN 2 AND 300),   -- invoice title (unvan)
  tax_office            text CONSTRAINT legal_parties_tax_office_chk CHECK (char_length(tax_office) <= 100),
  tax_no                text CONSTRAINT legal_parties_tax_no_chk CHECK (tax_no ~ '^[0-9]{10}$'),          -- VKN
  tckn_enc              bytea,                                                                          -- individual: TCKN, envelope-encrypted
  tckn_bidx             bytea CONSTRAINT legal_parties_tckn_bidx_chk CHECK (octet_length(tckn_bidx) = 32),
  mersis_no             text CONSTRAINT legal_parties_mersis_no_chk CHECK (mersis_no ~ '^[0-9]{16}$'),
  address               jsonb NOT NULL DEFAULT '{}',      -- {"line1","district","city","postal_code","country":"TR"}
  email                 text CONSTRAINT legal_parties_email_chk CHECK (char_length(email) <= 254),   -- invoice / payout contact
  phone                 text CONSTRAINT legal_parties_phone_chk CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
  vat_status            text NOT NULL DEFAULT 'unknown' CONSTRAINT legal_parties_vat_status_chk CHECK (vat_status IN ('vat_payer','vat_exempt','not_vat_payer','unknown')),
  e_invoice_alias       text,
  e_invoice_checked_at  timestamptz,
  status                text NOT NULL DEFAULT 'draft' CONSTRAINT legal_parties_status_chk CHECK (status IN ('draft','pending_verification','verified','rejected','inactive')),
  verified_at           timestamptz,
  verified_by           uuid CONSTRAINT legal_parties_verified_by_fkey REFERENCES users (id),
  created_by            uuid CONSTRAINT legal_parties_created_by_fkey REFERENCES users (id),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT legal_parties_id_kind_uq UNIQUE (id, kind),
  CONSTRAINT legal_parties_id_org_uq UNIQUE (id, org_id),
  CONSTRAINT legal_parties_address_chk CHECK (jsonb_typeof(address) = 'object'),
  CONSTRAINT legal_parties_tckn_chk CHECK ((legal_type = 'individual') = (tckn_enc IS NOT NULL)
                                       AND (legal_type = 'individual') = (tckn_bidx IS NOT NULL)),
  CONSTRAINT legal_parties_verified_tax_no_chk CHECK (status <> 'verified' OR legal_type = 'individual' OR tax_no IS NOT NULL),
  CONSTRAINT legal_parties_verified_chk CHECK ((status = 'verified') = (verified_at IS NOT NULL)
                                           AND (status = 'verified') = (verified_by IS NOT NULL)),
  CONSTRAINT legal_parties_buyer_org_chk CHECK (kind <> 'buyer_company' OR org_id IS NOT NULL)
);
CREATE UNIQUE INDEX legal_parties_platform_uq ON legal_parties (kind) WHERE kind = 'platform';
CREATE UNIQUE INDEX legal_parties_taxno_uq    ON legal_parties (kind, tax_no) WHERE tax_no IS NOT NULL AND status <> 'inactive';
CREATE UNIQUE INDEX legal_parties_tckn_uq     ON legal_parties (kind, tckn_bidx) WHERE tckn_bidx IS NOT NULL AND status <> 'inactive';
CREATE INDEX legal_parties_org_idx ON legal_parties (org_id) WHERE org_id IS NOT NULL;
CREATE TRIGGER legal_parties_touch_updated_at BEFORE UPDATE ON legal_parties
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON legal_parties TO app_rw;
COMMIT;
