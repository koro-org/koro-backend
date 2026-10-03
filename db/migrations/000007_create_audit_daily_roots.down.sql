-- 000007_create_audit_daily_roots.down.sql
-- DATA LOSS: signed daily roots of the audit chain.
BEGIN;
DROP TABLE IF EXISTS audit_daily_roots;
COMMIT;
