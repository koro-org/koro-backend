-- 000043_create_skills.up.sql (02 §8.7). The skill catalogue (loaded by
-- `koro seed reference`); rows are deactivated, never deleted.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE skills (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  slug        text NOT NULL CONSTRAINT skills_slug_uq UNIQUE CONSTRAINT skills_slug_chk CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
  name_tr     text COLLATE public.tr NOT NULL,
  name_en     text,
  category    text NOT NULL CONSTRAINT skills_category_chk CHECK (category IN ('technical','tool','soft','domain','language_tool','other')),
  is_active   boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX skills_trgm_idx ON skills USING gin (public.koro_norm(name_tr) public.gin_trgm_ops);
CREATE TRIGGER skills_touch_updated_at BEFORE UPDATE ON skills
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON skills TO app_rw;
COMMIT;
