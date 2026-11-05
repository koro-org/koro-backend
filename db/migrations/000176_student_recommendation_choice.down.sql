-- 000176_student_recommendation_choice.down.sql
-- DATA LOSS: the students' recommendation choices (BE-174).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE student_privacy_settings DROP COLUMN recommendation_choice;
COMMIT;
