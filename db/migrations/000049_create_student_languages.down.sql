-- 000049_create_student_languages.down.sql
-- DATA LOSS: the languages of every student.
BEGIN;
DROP TABLE IF EXISTS student_languages;
COMMIT;
