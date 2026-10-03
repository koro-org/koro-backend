-- 000100_create_commerce_policies.down.sql
-- DATA LOSS: every commerce policy (models, commission rates, history).
BEGIN;
DROP TABLE IF EXISTS commerce_policies;
COMMIT;
