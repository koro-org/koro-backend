-- 000089_create_retention_policies.down.sql
-- DATA LOSS: every retention policy (with legal approvals and edits).
BEGIN;
DROP TABLE IF EXISTS retention_policies;
COMMIT;
