-- 000118_create_provider_contact_map.up.sql (02 §22.3). The contact id of a
-- legal party at an invoicing / payment provider (Paraşüt, Nilvera, iyzico).
-- A cache: hard delete.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE provider_contact_map (
  legal_party_id       uuid NOT NULL CONSTRAINT provider_contact_map_legal_party_id_fkey REFERENCES legal_parties (id),
  provider             text NOT NULL CONSTRAINT provider_contact_map_provider_chk CHECK (provider IN ('parasut','nilvera','iyzico')),
  provider_contact_id  text NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT provider_contact_map_pkey PRIMARY KEY (legal_party_id, provider)
);
CREATE TRIGGER provider_contact_map_touch_updated_at BEFORE UPDATE ON provider_contact_map
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON provider_contact_map TO app_rw;
COMMIT;
