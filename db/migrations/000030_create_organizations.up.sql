-- 000030_create_organizations.up.sql (02 §7.1). The org root: platform,
-- university, club, company and (phase 2) ngo. Orgs are closed, never
-- deleted (status = 'closed'), so app_rw has no DELETE. UNIQUE (id, kind)
-- is the target of the composite (org_id, org_kind) FKs that bind a table to
-- one kind of org (02 §0.5).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE organizations (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  kind          text NOT NULL CONSTRAINT organizations_kind_chk CHECK (kind IN ('platform','university','club','company','ngo')),  -- ngo: phase 2, the v1 API does not create it
  name          text COLLATE public.tr NOT NULL CONSTRAINT organizations_name_chk CHECK (char_length(name) BETWEEN 2 AND 200),
  slug          text NOT NULL CONSTRAINT organizations_slug_uq UNIQUE CONSTRAINT organizations_slug_chk CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) <= 80),
  status        text NOT NULL DEFAULT 'active' CONSTRAINT organizations_status_chk CHECK (status IN ('pending_review','active','suspended','closed')),
  description   text CONSTRAINT organizations_description_chk CHECK (char_length(description) <= 5000),
  logo_file_id  uuid CONSTRAINT organizations_logo_file_id_fkey REFERENCES files (id),
  website_url   text CONSTRAINT organizations_website_url_chk CHECK (website_url ~ '^https://' AND char_length(website_url) <= 2048),
  city          text COLLATE public.tr CONSTRAINT organizations_city_chk CHECK (char_length(city) <= 60),
  verified_at   timestamptz,
  verified_by   uuid CONSTRAINT organizations_verified_by_fkey REFERENCES users (id),
  closed_at     timestamptz,
  created_by    uuid CONSTRAINT organizations_created_by_fkey REFERENCES users (id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  search_vector tsvector GENERATED ALWAYS AS
                (to_tsvector('public.koro_tr'::regconfig, coalesce(name, '') || ' ' || coalesce(description, ''))) STORED,
  CONSTRAINT organizations_id_kind_uq UNIQUE (id, kind),
  CONSTRAINT organizations_closed_chk CHECK ((status = 'closed') = (closed_at IS NOT NULL))
);
CREATE UNIQUE INDEX organizations_single_platform_uq ON organizations (kind) WHERE kind = 'platform';
CREATE INDEX organizations_kind_status_idx ON organizations (kind, status);
CREATE INDEX organizations_fts_idx  ON organizations USING gin (search_vector);
CREATE INDEX organizations_trgm_idx ON organizations USING gin (public.koro_norm(name) public.gin_trgm_ops);
CREATE TRIGGER organizations_touch_updated_at BEFORE UPDATE ON organizations
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON organizations TO app_rw;
COMMIT;
