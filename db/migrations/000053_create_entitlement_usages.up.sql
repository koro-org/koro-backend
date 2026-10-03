-- 000053_create_entitlement_usages.up.sql (02 §10.2). Append-only usage
-- ledger of an entitlement: a consumption is a positive row, its reversal a
-- negative row pointing at it (at most one reversal per usage). Two layers
-- (02 §0.5): app_rw has SELECT/INSERT only, and koro_forbid_mutation blocks
-- UPDATE/DELETE/TRUNCATE for everyone, the owner included. On a reversal
-- entitlements.quantity_used is decreased in the same transaction.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE entitlement_usages (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  entitlement_id  uuid NOT NULL CONSTRAINT entitlement_usages_entitlement_id_fkey REFERENCES entitlements (id),
  org_id          uuid NOT NULL CONSTRAINT entitlement_usages_org_id_fkey REFERENCES organizations (id),
  quantity        integer NOT NULL CONSTRAINT entitlement_usages_quantity_chk CHECK (quantity <> 0),
  ref_type        text NOT NULL CONSTRAINT entitlement_usages_ref_type_chk CHECK (ref_type IN ('post','dispatch','contact_request')),
  ref_id          uuid NOT NULL,
  reversal_of     uuid CONSTRAINT entitlement_usages_reversal_of_fkey REFERENCES entitlement_usages (id),
  actor_user_id   uuid CONSTRAINT entitlement_usages_actor_user_id_fkey REFERENCES users (id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT entitlement_usages_reversal_chk CHECK ((reversal_of IS NULL) = (quantity > 0))
);
CREATE UNIQUE INDEX entitlement_usages_ref_uq      ON entitlement_usages (ref_type, ref_id) WHERE reversal_of IS NULL;
CREATE UNIQUE INDEX entitlement_usages_reversal_uq ON entitlement_usages (reversal_of) WHERE reversal_of IS NOT NULL;
CREATE INDEX entitlement_usages_ent_idx ON entitlement_usages (entitlement_id, created_at);
CREATE TRIGGER entitlement_usages_immutable BEFORE UPDATE OR DELETE ON entitlement_usages
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER entitlement_usages_no_truncate BEFORE TRUNCATE ON entitlement_usages
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON entitlement_usages TO app_rw;
COMMIT;
