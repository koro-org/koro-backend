-- 000190_university_email_domains_outside_edu_tr.up.sql (BE-220; saha testi
-- B-16). A university's student addresses are not always under .edu.tr
-- (Sabancı Üniversitesi: sabanciuniv.edu; İstanbul Bilgi Üniversitesi:
-- bilgiedu.net), so the domain CHECK keeps the shape only: lower-case ASCII
-- labels, an alphabetic top-level label, at most 253 characters. Every
-- .edu.tr domain of the old CHECK still passes. Which domains may be stored
-- is the service's rule (directory domainRule: no public suffix such as
-- edu.tr, no shared mail service); the rows stay the allow-list, written
-- only by `koro seed reference` and adminPutUniversityEmailDomain.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE university_email_domains
  DROP CONSTRAINT university_email_domains_domain_chk,
  ADD CONSTRAINT university_email_domains_domain_chk CHECK (domain ~ '^([a-z0-9-]+\.)+[a-z]{2,63}$' AND char_length(domain) <= 253);
COMMIT;
