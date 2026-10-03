-- 000048_create_student_experiences.up.sql (02 §9.4). Experience entries.
-- 'club_role' entries may name a sensitive club: the company DTO shows them
-- only with show_clubs_on_grant (application). Deleted with the account.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE student_experiences (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id            uuid NOT NULL CONSTRAINT student_experiences_user_id_fkey REFERENCES users (id),
  kind               text NOT NULL CONSTRAINT student_experiences_kind_chk CHECK (kind IN ('internship','job','volunteer','project','club_role')),
  title              text NOT NULL CONSTRAINT student_experiences_title_chk CHECK (char_length(title) <= 120),
  organization_name  text CONSTRAINT student_experiences_organization_name_chk CHECK (char_length(organization_name) <= 160),
  start_month        date NOT NULL CONSTRAINT student_experiences_start_month_chk CHECK (extract(day FROM start_month) = 1),
  end_month          date CONSTRAINT student_experiences_end_month_chk CHECK (extract(day FROM end_month) = 1),
  description        text CONSTRAINT student_experiences_description_chk CHECK (char_length(description) <= 2000),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT student_experiences_period_chk CHECK (end_month IS NULL OR end_month >= start_month)
);
CREATE INDEX student_experiences_user_idx ON student_experiences (user_id, start_month DESC);
CREATE TRIGGER student_experiences_touch_updated_at BEFORE UPDATE ON student_experiences
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON student_experiences TO app_rw;
COMMIT;
