-- 000009_create_files.down.sql
-- DATA LOSS: file metadata (the objects in storage are orphaned).
BEGIN;
DROP TABLE IF EXISTS files;
COMMIT;
