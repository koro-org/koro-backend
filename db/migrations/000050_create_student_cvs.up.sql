-- 000050_create_student_cvs.up.sql (02 §9.6). CV files of a student (at most
-- 3 per user: application); at most one is current. Every view/download by a
-- company is audited (student.cv.view). Deleted with the account, the file
-- included.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE student_cvs (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id     uuid NOT NULL CONSTRAINT student_cvs_user_id_fkey REFERENCES users (id),
  file_id     uuid NOT NULL CONSTRAINT student_cvs_file_id_uq UNIQUE CONSTRAINT student_cvs_file_id_fkey REFERENCES files (id),
  is_current  boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX student_cvs_current_uq ON student_cvs (user_id) WHERE is_current;
GRANT SELECT, INSERT, UPDATE, DELETE ON student_cvs TO app_rw;
COMMIT;
