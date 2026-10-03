-- 000118_create_provider_contact_map.down.sql
-- DATA LOSS: the provider contact ids (rebuilt by the next sync).
BEGIN;
DROP TABLE IF EXISTS provider_contact_map;
COMMIT;
