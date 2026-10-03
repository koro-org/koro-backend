-- 000096_create_payees.up.sql (02 §17.3). The payee (hakediş sahibi) of a
-- seller org: the legal party (kind 'payee') that is paid on behalf of a club
-- or university. An org has at most one active default payee and one open
-- payee row per legal party. Orders freeze the payee (order_lines.payee_id).
-- Soft delete (status 'closed'); app_rw has no DELETE (financial_records).
-- 02 additions (H-20):
--   * payees_verified_pair_chk (06 H-37): verified_at and verified_by are set
--     together (02 only required them for 'active').
--   * payees_id_org_uq (06 H-38): the target of the composite FKs
--     (payee_id, org_id) of payee_bank_accounts, offerings, order_lines and
--     payouts, so a payee is only ever used for its own seller org.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE payees (
  id                       uuid PRIMARY KEY DEFAULT uuidv7(),
  legal_party_id           uuid NOT NULL,
  party_kind               text NOT NULL DEFAULT 'payee' CONSTRAINT payees_party_kind_chk CHECK (party_kind = 'payee'),
  org_id                   uuid NOT NULL CONSTRAINT payees_org_id_fkey REFERENCES organizations (id),   -- the seller org (club / university) paid on behalf of
  status                   text NOT NULL DEFAULT 'draft'
                             CONSTRAINT payees_status_chk CHECK (status IN ('draft','pending_verification','active','suspended','closed')),
  is_default               boolean NOT NULL DEFAULT false,
  payout_method            text NOT NULL DEFAULT 'bank_eft' CONSTRAINT payees_payout_method_chk CHECK (payout_method IN ('bank_eft','iyzico','none')),
  invoicing_capability     text NOT NULL DEFAULT 'unknown' CONSTRAINT payees_invoicing_capability_chk CHECK (invoicing_capability IN ('can_issue','cannot_issue','unknown')),
  university_protocol_ref  text CONSTRAINT payees_university_protocol_ref_chk CHECK (char_length(university_protocol_ref) <= 200),
  iyzico_submerchant_key   text,             -- phase 2
  iyzico_submerchant_type  text,             -- phase 2
  iyzico_status            text,             -- phase 2
  verified_at              timestamptz,
  verified_by              uuid CONSTRAINT payees_verified_by_fkey REFERENCES users (id),
  valid_from               timestamptz NOT NULL DEFAULT now(),
  valid_to                 timestamptz,
  created_by               uuid NOT NULL CONSTRAINT payees_created_by_fkey REFERENCES users (id),
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payees_legal_party_id_party_kind_fkey FOREIGN KEY (legal_party_id, party_kind) REFERENCES legal_parties (id, kind),
  CONSTRAINT payees_id_org_uq UNIQUE (id, org_id),
  CONSTRAINT payees_verified_pair_chk CHECK ((verified_at IS NULL) = (verified_by IS NULL)),
  CONSTRAINT payees_active_chk CHECK (status <> 'active' OR verified_at IS NOT NULL),
  CONSTRAINT payees_validity_chk CHECK (valid_to IS NULL OR valid_to > valid_from),
  CONSTRAINT payees_iyzico_chk CHECK (payout_method <> 'iyzico' OR iyzico_submerchant_key IS NOT NULL)
);
CREATE UNIQUE INDEX payees_default_uq   ON payees (org_id) WHERE is_default AND status = 'active';
CREATE UNIQUE INDEX payees_org_party_uq ON payees (org_id, legal_party_id) WHERE status <> 'closed';
CREATE TRIGGER payees_touch_updated_at BEFORE UPDATE ON payees
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON payees TO app_rw;
COMMIT;
