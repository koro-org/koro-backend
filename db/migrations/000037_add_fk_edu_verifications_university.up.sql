-- 000037_add_fk_edu_verifications_university.up.sql (02 §8.1). The forward FK
-- of edu_verifications (000021) now that universities exists.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE edu_verifications ADD CONSTRAINT edu_verifications_university_id_fkey
  FOREIGN KEY (university_id) REFERENCES universities (id);
COMMIT;
