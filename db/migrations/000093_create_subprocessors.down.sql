-- 000093_create_subprocessors.down.sql
-- DATA LOSS: the sub-processor inventory.
BEGIN;
DROP TABLE IF EXISTS subprocessors;
COMMIT;
