-- 000037_add_fk_edu_verifications_university.down.sql
BEGIN;
ALTER TABLE edu_verifications DROP CONSTRAINT IF EXISTS edu_verifications_university_id_fkey;
COMMIT;
