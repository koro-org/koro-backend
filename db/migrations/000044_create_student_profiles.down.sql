-- 000044_create_student_profiles.down.sql
-- DATA LOSS: every student profile.
BEGIN;
DROP TABLE IF EXISTS student_profiles;
COMMIT;
