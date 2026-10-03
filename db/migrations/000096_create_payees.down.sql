-- 000096_create_payees.down.sql
-- DATA LOSS: every payee of every seller org.
BEGIN;
DROP TABLE IF EXISTS payees;
COMMIT;
