-- 000049_create_student_languages.up.sql (02 §9.5). Languages of a student
-- (ISO 639 code and CEFR level). Deleted with the account.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE student_languages (
  user_id        uuid NOT NULL CONSTRAINT student_languages_user_id_fkey REFERENCES users (id),
  language_code  text NOT NULL CONSTRAINT student_languages_language_code_chk CHECK (language_code ~ '^[a-z]{2,3}$'),   -- ISO 639
  level          text NOT NULL CONSTRAINT student_languages_level_chk CHECK (level IN ('A1','A2','B1','B2','C1','C2','native')),
  created_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, language_code)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON student_languages TO app_rw;
COMMIT;
