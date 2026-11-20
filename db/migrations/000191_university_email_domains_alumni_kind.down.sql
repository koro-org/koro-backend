-- 000191_university_email_domains_alumni_kind.down.sql (BE-228).
-- Fails while an 'alumni' row is stored (app_rw cannot delete one): a
-- platform admin sets its kind to 'staff' first (the same meaning for
-- student verification). Roll back only together with the code that
-- writes the kind.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE university_email_domains
  DROP CONSTRAINT university_email_domains_domain_kind_chk,
  ADD CONSTRAINT university_email_domains_domain_kind_chk CHECK (domain_kind IN ('student','staff','mixed'));
COMMIT;
