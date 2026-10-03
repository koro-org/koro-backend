-- 000046_create_student_privacy_settings.down.sql
-- DATA LOSS: the privacy choices of every student.
BEGIN;
DROP TABLE IF EXISTS student_privacy_settings;
COMMIT;
