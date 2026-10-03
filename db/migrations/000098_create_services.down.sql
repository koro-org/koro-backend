-- 000098_create_services.down.sql
-- DATA LOSS: the service catalogue.
BEGIN;
DROP TABLE IF EXISTS services;
COMMIT;
