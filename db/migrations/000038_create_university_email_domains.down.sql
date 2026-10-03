-- 000038_create_university_email_domains.down.sql
-- DATA LOSS: the e-mail domain mapping (reloaded by `koro seed reference`).
BEGIN;
DROP TABLE IF EXISTS university_email_domains;
COMMIT;
