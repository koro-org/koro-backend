-- 000044_create_student_profiles.up.sql (02 §9.1). The student profile, 1:1
-- with the user. audience_keys / audience_eligible are the targeting keys of
-- the feed and outreach (maintained by the students module); count_segment
-- (000045) counts over them. Deleted with the account (hard delete).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE student_profiles (
  user_id                uuid PRIMARY KEY CONSTRAINT student_profiles_user_id_fkey REFERENCES users (id),
  university_id          uuid NOT NULL CONSTRAINT student_profiles_university_id_fkey REFERENCES universities (id),   -- same as the active verification (application)
  department_id          uuid CONSTRAINT student_profiles_department_id_fkey REFERENCES departments (id),
  degree_level           text NOT NULL CONSTRAINT student_profiles_degree_level_chk CHECK (degree_level IN ('associate','bachelor','master','doctorate')),
  class_year             smallint CONSTRAINT student_profiles_class_year_chk CHECK (class_year BETWEEN 1 AND 8),
  expected_graduation    date CONSTRAINT student_profiles_expected_graduation_chk CHECK (extract(day FROM expected_graduation) = 1),
  status                 text NOT NULL DEFAULT 'active' CONSTRAINT student_profiles_status_chk CHECK (status IN ('active','graduated','inactive')),
  graduated_at           timestamptz,
  headline               text CONSTRAINT student_profiles_headline_chk CHECK (char_length(headline) <= 160),
  about                  text CONSTRAINT student_profiles_about_chk CHECK (char_length(about) <= 2000),
  city                   text COLLATE public.tr CONSTRAINT student_profiles_city_chk CHECK (char_length(city) <= 60),
  avatar_file_id         uuid CONSTRAINT student_profiles_avatar_file_id_fkey REFERENCES files (id),
  completed_sections     text[] NOT NULL DEFAULT '{}'
                           CONSTRAINT student_profiles_completed_sections_chk CHECK (completed_sections <@ ARRAY['basics','skills','experience','languages','cv']::text[]),
  audience_keys          text[] NOT NULL DEFAULT '{}',    -- 'all','uni:<id>','dept:<id>','year:<n>','club:<id>' (club: ONLY non-sensitive clubs)
  audience_eligible      boolean NOT NULL DEFAULT false,  -- active verification AND adult AND status = 'active' AND users.status = 'active'
  audience_refreshed_at  timestamptz,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT student_profiles_graduated_chk CHECK ((status = 'graduated') = (graduated_at IS NOT NULL))
);
CREATE INDEX student_profiles_audience_idx ON student_profiles USING gin (audience_keys) WHERE audience_eligible;
CREATE INDEX student_profiles_filter_idx   ON student_profiles (university_id, department_id, class_year) WHERE status = 'active';
CREATE TRIGGER student_profiles_touch_updated_at BEFORE UPDATE ON student_profiles
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON student_profiles TO app_rw;
COMMIT;
