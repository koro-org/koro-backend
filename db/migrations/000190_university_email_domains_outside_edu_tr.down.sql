-- 000190_university_email_domains_outside_edu_tr.down.sql (BE-220).
-- Fails while a domain outside .edu.tr is stored (app_rw cannot delete
-- one): a platform admin deactivates it first and the owner removes the row
-- by hand, knowingly. Roll back only together with the code that writes them.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE university_email_domains
  DROP CONSTRAINT university_email_domains_domain_chk,
  ADD CONSTRAINT university_email_domains_domain_chk CHECK (domain ~ '^([a-z0-9-]+\.)+edu\.tr$');
COMMIT;
