-- 000047_create_student_skills.up.sql (02 §9.3). Skills of a student (at most
-- 30 per user: application). Deleted with the account.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE student_skills (
  user_id     uuid NOT NULL CONSTRAINT student_skills_user_id_fkey REFERENCES users (id),
  skill_id    uuid NOT NULL CONSTRAINT student_skills_skill_id_fkey REFERENCES skills (id),
  level       smallint CONSTRAINT student_skills_level_chk CHECK (level BETWEEN 1 AND 5),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, skill_id)
);
CREATE INDEX student_skills_skill_idx ON student_skills (skill_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON student_skills TO app_rw;
COMMIT;
