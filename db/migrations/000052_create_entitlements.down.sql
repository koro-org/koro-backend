-- 000052_create_entitlements.down.sql
-- DATA LOSS: every entitlement (purchased rights of companies).
BEGIN;
DROP TABLE IF EXISTS entitlements;
COMMIT;
