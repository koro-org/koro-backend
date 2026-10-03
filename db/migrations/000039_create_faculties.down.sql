-- 000039_create_faculties.down.sql
-- DATA LOSS: the faculty catalogue (reloaded by `koro seed reference`).
BEGIN;
DROP TABLE IF EXISTS faculties;
COMMIT;
