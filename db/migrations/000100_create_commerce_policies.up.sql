-- 000100_create_commerce_policies.up.sql (02 §18.3; ADR-0014). The commerce
-- policy: payment model (A marketplace — the MVP bank-transfer flow — A2, B,
-- C), collection method and commission, by scope (offering > service >
-- seller_org > default). At order time the valid row is copied into
-- order_lines, so a later change never touches an order. Rows are immutable
-- except closing (valid_to NULL → value, koro_policy_close_only); a change is
-- "close + new row". Validity periods of one scope cannot overlap: the
-- exclusion constraint (btree_gist, 000001; T-43) rejects it with 23P01,
-- also between concurrent transactions. The MVP default row (model A,
-- commission rate a team decision) is written by `koro seed
-- bootstrap-commerce` (BE-038), not here.
-- 02 note (H-20): the exclusion constraint is named
-- commerce_policies_no_overlap_uq (its index follows the 02 §23 _uq naming);
-- commerce_policies_lookup_idx serves the policy resolution.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE commerce_policies (
  id                      uuid PRIMARY KEY DEFAULT uuidv7(),
  scope                   text NOT NULL CONSTRAINT commerce_policies_scope_chk CHECK (scope IN ('default','seller_org','service','offering')),
  scope_id                uuid,
  model_code              text NOT NULL CONSTRAINT commerce_policies_model_code_chk CHECK (model_code IN ('A','A2','B','C')),
  collection_method       text NOT NULL CONSTRAINT commerce_policies_collection_method_chk CHECK (collection_method IN ('bank_transfer','iyzico','direct_to_payee')),
  commission_rate_bp      integer NOT NULL CONSTRAINT commerce_policies_commission_rate_bp_chk CHECK (commission_rate_bp BETWEEN 0 AND 10000),
  commission_min_amount   bigint CONSTRAINT commerce_policies_commission_min_amount_chk CHECK (commission_min_amount >= 0),
  commission_max_amount   bigint CONSTRAINT commerce_policies_commission_max_amount_chk CHECK (commission_max_amount >= 0),
  commission_basis        text NOT NULL CONSTRAINT commerce_policies_commission_basis_chk CHECK (commission_basis IN ('net_excl_vat','gross_incl_vat')),
  commission_payer        text NOT NULL CONSTRAINT commerce_policies_commission_payer_chk CHECK (commission_payer IN ('seller','buyer')),
  commission_vat_rate_bp  integer NOT NULL DEFAULT 2000 CONSTRAINT commerce_policies_commission_vat_rate_bp_chk CHECK (commission_vat_rate_bp BETWEEN 0 AND 10000),
  invoice_automation      text NOT NULL DEFAULT 'manual' CONSTRAINT commerce_policies_invoice_automation_chk CHECK (invoice_automation IN ('manual','provider')),
  dispute_window_days     smallint NOT NULL DEFAULT 7 CONSTRAINT commerce_policies_dispute_window_days_chk CHECK (dispute_window_days BETWEEN 0 AND 60),
  payout_hold_days        smallint NOT NULL DEFAULT 0 CONSTRAINT commerce_policies_payout_hold_days_chk CHECK (payout_hold_days BETWEEN 0 AND 90),
  valid_from              timestamptz NOT NULL,
  valid_to                timestamptz,
  notes                   text CONSTRAINT commerce_policies_notes_chk CHECK (char_length(notes) <= 1000),
  created_by              uuid NOT NULL CONSTRAINT commerce_policies_created_by_fkey REFERENCES users (id),
  created_at              timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT commerce_policies_scope_id_chk CHECK ((scope = 'default') = (scope_id IS NULL)),
  CONSTRAINT commerce_policies_model_method_chk CHECK ((model_code = 'C') = (collection_method = 'direct_to_payee')),
  CONSTRAINT commerce_policies_model_c_payer_chk CHECK (model_code <> 'C' OR commission_payer = 'buyer'),
  CONSTRAINT commerce_policies_commission_range_chk CHECK (commission_min_amount IS NULL OR commission_max_amount IS NULL
                                                         OR commission_min_amount <= commission_max_amount),
  CONSTRAINT commerce_policies_validity_chk CHECK (valid_to IS NULL OR valid_to > valid_from),
  CONSTRAINT commerce_policies_no_overlap_uq EXCLUDE USING gist (
    scope WITH =,
    (coalesce(scope_id, '00000000-0000-0000-0000-000000000000'::uuid)) WITH =,
    tstzrange(valid_from, coalesce(valid_to, 'infinity'::timestamptz), '[)') WITH &&
  )
);
CREATE INDEX commerce_policies_lookup_idx ON commerce_policies (scope, scope_id, valid_from DESC);
CREATE TRIGGER commerce_policies_close_only BEFORE UPDATE OR DELETE ON commerce_policies
  FOR EACH ROW EXECUTE FUNCTION public.koro_policy_close_only();
CREATE TRIGGER commerce_policies_no_truncate BEFORE TRUNCATE ON commerce_policies
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT, UPDATE ON commerce_policies TO app_rw;
COMMIT;
