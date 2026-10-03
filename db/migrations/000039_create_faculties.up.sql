-- 000039_create_faculties.up.sql (02 §8.3).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE faculties (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  university_id  uuid NOT NULL CONSTRAINT faculties_university_id_fkey REFERENCES universities (id),
  name           text COLLATE public.tr NOT NULL,
  yok_code       text,
  is_active      boolean NOT NULL DEFAULT true,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT faculties_university_name_uq UNIQUE (university_id, name)
);
CREATE TRIGGER faculties_touch_updated_at BEFORE UPDATE ON faculties
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON faculties TO app_rw;
COMMIT;
