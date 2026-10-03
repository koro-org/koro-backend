-- 000050_create_student_cvs.down.sql
-- DATA LOSS: the CV records of every student (the files stay in storage).
BEGIN;
DROP TABLE IF EXISTS student_cvs;
COMMIT;
