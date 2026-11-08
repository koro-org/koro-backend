-- 000184_student_interests.down.sql (BE-204).
-- DATA LOSS: every student's interests. Roll back only together with the code
-- that uses them.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE student_profiles DROP COLUMN interests;
COMMIT;
