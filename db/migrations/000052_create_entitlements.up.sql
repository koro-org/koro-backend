-- 000052_create_entitlements.up.sql (02 §10.1). A right a company holds
-- (quota of targeted posts / dispatches / contact requests, or access to
-- discovery). Consumption is a conditional UPDATE guarded by status, the
-- validity window and quantity_used <= quantity_total; the CHECKs below are
-- the last line. source_id has no FK (layer rule: order_lines are a later
-- module). Hard-deleted after the retention period; while entitlement_usages
-- rows exist the FK keeps the entitlement (the purge of the immutable usages
-- is T-120).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE entitlements (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id          uuid NOT NULL CONSTRAINT entitlements_org_id_fkey REFERENCES organizations (id),   -- the holder (a company)
  feature         text NOT NULL CONSTRAINT entitlements_feature_chk CHECK (feature IN ('targeted_post','targeted_dispatch','contact_request','discovery_access')),
  kind            text NOT NULL CONSTRAINT entitlements_kind_chk CHECK (kind IN ('quota','access')),
  quantity_total  integer CONSTRAINT entitlements_quantity_total_chk CHECK (quantity_total > 0),
  quantity_used   integer NOT NULL DEFAULT 0 CONSTRAINT entitlements_quantity_used_chk CHECK (quantity_used >= 0),
  valid_from      timestamptz NOT NULL,
  valid_to        timestamptz NOT NULL,
  status          text NOT NULL CONSTRAINT entitlements_status_chk CHECK (status IN ('pending','active','exhausted','expired','revoked')),
  source_type     text NOT NULL CONSTRAINT entitlements_source_type_chk CHECK (source_type IN ('order_line','admin_grant','promo')),
  source_id       uuid,                                          -- order_line_id etc.; no FK (layer rule)
  granted_by      uuid CONSTRAINT entitlements_granted_by_fkey REFERENCES users (id),
  activated_at    timestamptz,
  revoked_at      timestamptz,
  revoke_reason   text CONSTRAINT entitlements_revoke_reason_chk CHECK (char_length(revoke_reason) <= 300),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT entitlements_quota_chk CHECK ((kind = 'quota') = (quantity_total IS NOT NULL)),
  CONSTRAINT entitlements_access_feature_chk CHECK ((feature = 'discovery_access') = (kind = 'access')),
  CONSTRAINT entitlements_used_total_chk CHECK (quantity_total IS NULL OR quantity_used <= quantity_total),
  CONSTRAINT entitlements_validity_chk CHECK (valid_to > valid_from),
  CONSTRAINT entitlements_source_chk CHECK (source_type = 'admin_grant' OR source_id IS NOT NULL),
  CONSTRAINT entitlements_granted_by_chk CHECK (source_type <> 'admin_grant' OR granted_by IS NOT NULL),
  CONSTRAINT entitlements_activated_chk CHECK (status NOT IN ('active','exhausted') OR activated_at IS NOT NULL),
  CONSTRAINT entitlements_revoked_chk CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE INDEX entitlements_org_feature_idx ON entitlements (org_id, feature, valid_to) WHERE status = 'active';
CREATE UNIQUE INDEX entitlements_source_uq ON entitlements (source_type, source_id, feature) WHERE source_id IS NOT NULL;
CREATE INDEX entitlements_expiry_idx ON entitlements (valid_to) WHERE status = 'active';
CREATE TRIGGER entitlements_touch_updated_at BEFORE UPDATE ON entitlements
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON entitlements TO app_rw;
COMMIT;
