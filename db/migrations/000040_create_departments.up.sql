-- 000040_create_departments.up.sql (02 §8.4).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE departments (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  university_id     uuid NOT NULL CONSTRAINT departments_university_id_fkey REFERENCES universities (id),
  faculty_id        uuid CONSTRAINT departments_faculty_id_fkey REFERENCES faculties (id),
  name              text COLLATE public.tr NOT NULL,
  yok_program_code  text,
  degree_level      text NOT NULL CONSTRAINT departments_degree_level_chk CHECK (degree_level IN ('associate','bachelor','master','doctorate')),
  duration_years    smallint CONSTRAINT departments_duration_years_chk CHECK (duration_years BETWEEN 1 AND 7),
  is_active         boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX departments_yok_uq ON departments (university_id, yok_program_code) WHERE yok_program_code IS NOT NULL;
CREATE INDEX departments_uni_idx  ON departments (university_id) WHERE is_active;
CREATE INDEX departments_trgm_idx ON departments USING gin (public.koro_norm(name) public.gin_trgm_ops);
CREATE TRIGGER departments_touch_updated_at BEFORE UPDATE ON departments
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON departments TO app_rw;
COMMIT;
