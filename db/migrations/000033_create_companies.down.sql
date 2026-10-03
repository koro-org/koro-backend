-- 000033_create_companies.down.sql
-- DATA LOSS: company profile data (sector, size, verified e-mail domain).
BEGIN;
DROP TABLE IF EXISTS companies;
COMMIT;
