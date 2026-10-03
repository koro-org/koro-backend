-- 000120_create_integration_credentials.down.sql
-- DATA LOSS: the stored integration tokens (the integrations must be
-- connected again).
BEGIN;
DROP TABLE IF EXISTS integration_credentials;
COMMIT;
