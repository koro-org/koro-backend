-- 000099_create_offerings.up.sql (02 §18.2). An offering (teklif) of a seller
-- org (club / university) or of the platform: a priced service, optionally
-- bound to an event, with a stock (quantity_total / quantity_sold) that the
-- order reservation moves with one conditional UPDATE (02 §18.2). Prices are
-- kuruş, rates basis points; the VAT rate is copied from the service. An org
-- offering is sold only with a default payee; a platform admin other than
-- its creator approves it (03 §6.1); a university-bound offering may need
-- the university's decision. Soft delete ('cancelled' / 'closed'); only a
-- draft may be hard-deleted (offerings_draft_delete_only, like events).
-- 02 fixes and additions (H-20):
--   * 06 H-38: (default_payee_id, org_id) → payees: the default payee belongs
--     to the seller org; offerings_id_org_uq is the target of
--     order_lines (offering_id, seller_org_id).
--   * 06 H-55 (BE-038): (event_id, org_id) → events (id, org_id)
--     (events_id_org_uq): an offering is bound only to an event of its own
--     seller org; MATCH SIMPLE leaves an offering without an event free.
--   * 03 §6.1 "Teklif onayı" in the database as well: approved_by <> created_by.
--   * per-state CHECKs (06 H-37): approved_by/at together; pending_approval
--     has submitted_at; the university decision columns belong to a decision
--     and requires_university_approval matches the decision state; sold_out
--     means the stock is exhausted.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE offerings (
  id                           uuid PRIMARY KEY DEFAULT uuidv7(),
  service_id                   uuid NOT NULL CONSTRAINT offerings_service_id_fkey REFERENCES services (id),
  seller_kind                  text NOT NULL CONSTRAINT offerings_seller_kind_chk CHECK (seller_kind IN ('org','platform')),
  org_id                       uuid NOT NULL CONSTRAINT offerings_org_id_fkey REFERENCES organizations (id),   -- seller org (the platform org for platform offerings)
  event_id                     uuid,
  default_payee_id             uuid,
  title                        text COLLATE "tr" NOT NULL CONSTRAINT offerings_title_chk CHECK (char_length(title) BETWEEN 3 AND 160),
  description_json             jsonb,
  deliverables                 jsonb NOT NULL DEFAULT '[]',   -- [{"key":"logo_on_banner","label":"Afişte logo"}]
  unit_price_amount            bigint NOT NULL CONSTRAINT offerings_unit_price_amount_chk CHECK (unit_price_amount > 0),
  currency                     char(3) NOT NULL DEFAULT 'TRY' CONSTRAINT offerings_currency_chk CHECK (currency = 'TRY'),
  vat_rate_bp                  integer NOT NULL CONSTRAINT offerings_vat_rate_bp_chk CHECK (vat_rate_bp BETWEEN 0 AND 10000),   -- copied from the service
  compensation_type            text NOT NULL DEFAULT 'cash' CONSTRAINT offerings_compensation_type_chk CHECK (compensation_type IN ('cash','in_kind')),
  quantity_total               integer CONSTRAINT offerings_quantity_total_chk CHECK (quantity_total > 0),
  quantity_sold                integer NOT NULL DEFAULT 0 CONSTRAINT offerings_quantity_sold_chk CHECK (quantity_sold >= 0),
  per_buyer_limit              integer CONSTRAINT offerings_per_buyer_limit_chk CHECK (per_buyer_limit > 0),
  sales_start_at               timestamptz,
  sales_end_at                 timestamptz,
  fulfil_by                    timestamptz,
  invoice_trigger              text NOT NULL DEFAULT 'on_fulfillment' CONSTRAINT offerings_invoice_trigger_chk CHECK (invoice_trigger IN ('on_payment','on_fulfillment')),
  requires_university_approval boolean NOT NULL DEFAULT false,
  university_approval_status   text NOT NULL DEFAULT 'not_required'
                                 CONSTRAINT offerings_university_approval_status_chk CHECK (university_approval_status IN ('not_required','pending','approved','rejected')),
  university_approved_by       uuid CONSTRAINT offerings_university_approved_by_fkey REFERENCES users (id),
  university_approved_at       timestamptz,
  blocked_sponsor_flags        text[] NOT NULL DEFAULT '{}',    -- {'alcohol','tobacco','gambling','political'}
  status                       text NOT NULL DEFAULT 'draft'
                                 CONSTRAINT offerings_status_chk CHECK (status IN ('draft','pending_approval','active','sold_out','closed','cancelled')),
  submitted_at                 timestamptz,
  approved_by                  uuid CONSTRAINT offerings_approved_by_fkey REFERENCES users (id),   -- platform admin
  approved_at                  timestamptz,
  rejection_reason             text CONSTRAINT offerings_rejection_reason_chk CHECK (char_length(rejection_reason) <= 500),
  cancelled_at                 timestamptz,
  cancel_reason                text CONSTRAINT offerings_cancel_reason_chk CHECK (char_length(cancel_reason) <= 500),
  terms_version                text,
  created_by                   uuid NOT NULL CONSTRAINT offerings_created_by_fkey REFERENCES users (id),
  created_at                   timestamptz NOT NULL DEFAULT now(),
  updated_at                   timestamptz NOT NULL DEFAULT now(),
  search_vector                tsvector GENERATED ALWAYS AS (to_tsvector('public.koro_tr'::regconfig, coalesce(title, ''))) STORED,
  CONSTRAINT offerings_default_payee_id_org_id_fkey FOREIGN KEY (default_payee_id, org_id) REFERENCES payees (id, org_id),
  CONSTRAINT offerings_event_id_org_id_fkey FOREIGN KEY (event_id, org_id) REFERENCES events (id, org_id),
  CONSTRAINT offerings_id_org_uq UNIQUE (id, org_id),
  CONSTRAINT offerings_stock_chk CHECK (quantity_total IS NULL OR quantity_sold <= quantity_total),
  CONSTRAINT offerings_sold_out_chk CHECK (status <> 'sold_out' OR (quantity_total IS NOT NULL AND quantity_sold = quantity_total)),
  CONSTRAINT offerings_sales_window_chk CHECK (sales_end_at IS NULL OR sales_start_at IS NULL OR sales_end_at > sales_start_at),
  CONSTRAINT offerings_payee_required_chk CHECK (seller_kind = 'platform' OR status IN ('draft','cancelled') OR default_payee_id IS NOT NULL),
  CONSTRAINT offerings_platform_payee_chk CHECK (seller_kind = 'org' OR default_payee_id IS NULL),
  CONSTRAINT offerings_submitted_chk CHECK (status <> 'pending_approval' OR submitted_at IS NOT NULL),
  CONSTRAINT offerings_approved_pair_chk CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
  CONSTRAINT offerings_approved_chk CHECK (status NOT IN ('active','sold_out','closed') OR approved_by IS NOT NULL),
  CONSTRAINT offerings_approver_chk CHECK (approved_by IS NULL OR approved_by <> created_by),
  CONSTRAINT offerings_university_required_chk CHECK (requires_university_approval = (university_approval_status <> 'not_required')),
  CONSTRAINT offerings_university_decision_chk CHECK ((university_approval_status IN ('approved','rejected')) = (university_approved_by IS NOT NULL)
                                                     AND (university_approval_status IN ('approved','rejected')) = (university_approved_at IS NOT NULL)),
  CONSTRAINT offerings_university_approval_chk CHECK (NOT requires_university_approval OR status NOT IN ('active','sold_out')
         OR university_approval_status = 'approved'),
  CONSTRAINT offerings_cancelled_chk CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
  CONSTRAINT offerings_in_kind_chk CHECK (compensation_type = 'cash' OR status IN ('draft','cancelled'))    -- in kind is not sold in v1
);
CREATE INDEX offerings_org_idx    ON offerings (org_id, status);
CREATE INDEX offerings_event_idx  ON offerings (event_id) WHERE event_id IS NOT NULL;
CREATE INDEX offerings_active_idx ON offerings (sales_end_at) WHERE status = 'active';
CREATE INDEX offerings_fts_idx    ON offerings USING gin (search_vector);
CREATE TRIGGER offerings_touch_updated_at BEFORE UPDATE ON offerings
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();

-- Only a draft may be deleted (02 §18.2 "taslak hard delete serbest").
CREATE FUNCTION public.offerings_forbid_nondraft_delete() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'DELETE on %.% is not allowed for a % offering: only a draft may be deleted', TG_TABLE_SCHEMA, TG_TABLE_NAME, OLD.status
    USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER offerings_draft_delete_only BEFORE DELETE ON offerings
  FOR EACH ROW WHEN (OLD.status <> 'draft') EXECUTE FUNCTION public.offerings_forbid_nondraft_delete();
GRANT SELECT, INSERT, UPDATE, DELETE ON offerings TO app_rw;
COMMIT;
