-- 000119_create_provider_product_map.down.sql
-- DATA LOSS: the provider product ids (rebuilt by the next sync).
BEGIN;
DROP TABLE IF EXISTS provider_product_map;
COMMIT;
