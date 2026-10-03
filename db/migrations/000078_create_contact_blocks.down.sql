-- 000078_create_contact_blocks.down.sql
-- DATA LOSS: every contact block.
BEGIN;
DROP TABLE IF EXISTS contact_blocks;
COMMIT;
