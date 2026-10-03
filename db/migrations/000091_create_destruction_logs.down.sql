-- 000091_create_destruction_logs.down.sql
-- DATA LOSS: the whole destruction record (KVKK imha kaydı).
BEGIN;
DROP TABLE IF EXISTS destruction_logs;
COMMIT;
