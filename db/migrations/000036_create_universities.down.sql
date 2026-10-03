-- 000036_create_universities.down.sql
-- DATA LOSS: the university catalogue (reloaded by `koro seed reference`).
BEGIN;
DROP TABLE IF EXISTS universities;
COMMIT;
