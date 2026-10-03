-- 000036_create_universities.up.sql (02 §8.1). One row per university,
-- 1:1 with an organizations row of kind 'university' (created together at
-- import, even when nobody is a member yet). Reference data is loaded by
-- `koro seed reference` (YÖK open data), not by a migration. Rows are
-- deactivated (is_active = false), never deleted, so app_rw has no DELETE.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE universities (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id        uuid NOT NULL CONSTRAINT universities_org_id_uq UNIQUE,
  org_kind      text NOT NULL DEFAULT 'university' CONSTRAINT universities_org_kind_chk CHECK (org_kind = 'university'),
  yok_code      text NOT NULL CONSTRAINT universities_yok_code_uq UNIQUE,
  name          text COLLATE public.tr NOT NULL,
  short_name    text COLLATE public.tr,
  city          text COLLATE public.tr NOT NULL,
  type          text NOT NULL CONSTRAINT universities_type_chk CHECK (type IN ('state','foundation','other')),
  website_url   text CONSTRAINT universities_website_url_chk CHECK (website_url ~ '^https?://'),
  is_active     boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  search_vector tsvector GENERATED ALWAYS AS
                (to_tsvector('public.koro_tr'::regconfig, coalesce(name, '') || ' ' || coalesce(short_name, '') || ' ' || city)) STORED,
  CONSTRAINT universities_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind)
);
CREATE INDEX universities_fts_idx  ON universities USING gin (search_vector);
CREATE INDEX universities_trgm_idx ON universities USING gin (public.koro_norm(name) public.gin_trgm_ops);
CREATE TRIGGER universities_touch_updated_at BEFORE UPDATE ON universities
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON universities TO app_rw;
COMMIT;
