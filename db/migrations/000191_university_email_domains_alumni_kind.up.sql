-- 000191_university_email_domains_alumni_kind.up.sql (BE-228; T-443).
-- Universities give graduates lifelong mailboxes under a subdomain of their
-- student domain (alumni.metu.edu.tr, alumni.bogazici.edu.tr,
-- alumni.sabanciuniv.edu). The kind 'alumni' records such a domain: like
-- 'staff' it never proves a student (directory StudentUniversityByEmailDomain
-- takes exactly a 'student' or 'mixed' row, and no subdomain of one), but it
-- still names the university's addresses. Existing rows keep their kind.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE university_email_domains
  DROP CONSTRAINT university_email_domains_domain_kind_chk,
  ADD CONSTRAINT university_email_domains_domain_kind_chk CHECK (domain_kind IN ('student','staff','mixed','alumni'));
COMMIT;
