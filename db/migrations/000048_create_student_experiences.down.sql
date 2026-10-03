-- 000048_create_student_experiences.down.sql
-- DATA LOSS: the experience entries of every student.
BEGIN;
DROP TABLE IF EXISTS student_experiences;
COMMIT;
