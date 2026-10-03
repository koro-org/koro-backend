-- 000053_create_entitlement_usages.down.sql
-- DATA LOSS: the whole usage ledger of the entitlements.
BEGIN;
DROP TABLE IF EXISTS entitlement_usages;
COMMIT;
