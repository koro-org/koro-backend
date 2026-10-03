-- 000119_create_provider_product_map.up.sql (02 §22.4). The product id of a
-- service at an invoicing provider. A cache: hard delete.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE provider_product_map (
  service_id           uuid NOT NULL CONSTRAINT provider_product_map_service_id_fkey REFERENCES services (id),
  provider             text NOT NULL CONSTRAINT provider_product_map_provider_chk CHECK (provider IN ('parasut','nilvera')),
  provider_product_id  text NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT provider_product_map_pkey PRIMARY KEY (service_id, provider)
);
CREATE TRIGGER provider_product_map_touch_updated_at BEFORE UPDATE ON provider_product_map
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON provider_product_map TO app_rw;
COMMIT;
