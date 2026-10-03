-- 000040_create_departments.down.sql
-- DATA LOSS: the department catalogue (reloaded by `koro seed reference`).
BEGIN;
DROP TABLE IF EXISTS departments;
COMMIT;
