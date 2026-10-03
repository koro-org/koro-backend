-- 000098_create_services.up.sql (02 §18.1). The service catalogue of Koro
-- (sponsorship, booth, logo placement, platform-native quotas ...): the
-- invoice wording, the default VAT rate (basis points) and the VAT class of
-- every offering. A service cannot be sold before the accountant's review
-- (accounting_reviewed_at; enforced by the catalog service). Soft delete
-- (active = false): app_rw has no DELETE.
-- 02 additions (H-20): services_name_tr_chk (2–160 characters) and
-- services_entitlement_template_chk (a JSON object).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE services (
  id                       uuid PRIMARY KEY DEFAULT uuidv7(),
  code                     text NOT NULL CONSTRAINT services_code_uq UNIQUE CONSTRAINT services_code_chk CHECK (code ~ '^[a-z][a-z0-9_]{2,40}$'),
  name_tr                  text COLLATE "tr" NOT NULL CONSTRAINT services_name_tr_chk CHECK (char_length(name_tr) BETWEEN 2 AND 160),
  description_tr           text CONSTRAINT services_description_tr_chk CHECK (char_length(description_tr) <= 2000),
  category                 text NOT NULL CONSTRAINT services_category_chk CHECK (category IN ('sponsorship','booth','logo_placement','talk_slot',
                             'social_media_post','workshop','targeted_post_quota','contact_request_quota',
                             'discovery_access','other')),
  fulfillment_type         text NOT NULL CONSTRAINT services_fulfillment_type_chk CHECK (fulfillment_type IN ('event_bound','org_bound','platform_native')),
  entitlement_template     jsonb,      -- platform_native: {"feature":"targeted_post","kind":"quota","quantity_per_unit":5,"valid_days":90}
  default_vat_rate_bp      integer NOT NULL DEFAULT 2000 CONSTRAINT services_default_vat_rate_bp_chk CHECK (default_vat_rate_bp BETWEEN 0 AND 10000),
  vat_class                text NOT NULL DEFAULT 'standard' CONSTRAINT services_vat_class_chk CHECK (vat_class IN ('standard','exempt','outside_scope','withheld')),
  vat_exemption_code       text,
  vat_withholding_code     text,
  vat_withholding_num      smallint,
  vat_withholding_den      smallint,
  invoice_description_tr   text CONSTRAINT services_invoice_description_tr_chk CHECK (char_length(invoice_description_tr) <= 300),
  accounting_reviewed_at   timestamptz,          -- accountant's review (NULL ⇒ cannot be sold)
  accounting_reviewed_ref  text,
  active                   boolean NOT NULL DEFAULT true,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT services_entitlement_template_chk CHECK ((fulfillment_type = 'platform_native') = (entitlement_template IS NOT NULL)
                                                   AND (entitlement_template IS NULL OR jsonb_typeof(entitlement_template) = 'object')),
  CONSTRAINT services_withholding_pair_chk CHECK ((vat_withholding_num IS NULL) = (vat_withholding_den IS NULL)),
  CONSTRAINT services_withholding_ratio_chk CHECK (vat_withholding_num IS NULL OR (vat_withholding_num > 0 AND vat_withholding_num <= vat_withholding_den)),
  CONSTRAINT services_withheld_class_chk CHECK ((vat_class = 'withheld') = (vat_withholding_num IS NOT NULL)),
  CONSTRAINT services_exemption_chk CHECK (vat_class <> 'exempt' OR vat_exemption_code IS NOT NULL)
);
CREATE TRIGGER services_touch_updated_at BEFORE UPDATE ON services
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON services TO app_rw;
COMMIT;
