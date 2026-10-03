-- 000010_create_consent_purposes.down.sql
-- DATA LOSS: the consent purposes (including edits made after the seed).
BEGIN;
DROP TABLE IF EXISTS consent_purposes;
COMMIT;
