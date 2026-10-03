-- 000038_create_university_email_domains.up.sql (02 §8.2). The .edu.tr
-- domains of a university; a domain matches an address when it equals the
-- address domain or is a dot-suffix of it (the longest match wins).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE university_email_domains (
  domain         text PRIMARY KEY CONSTRAINT university_email_domains_domain_chk CHECK (domain ~ '^([a-z0-9-]+\.)+edu\.tr$'),
  university_id  uuid NOT NULL CONSTRAINT university_email_domains_university_id_fkey REFERENCES universities (id),
  domain_kind    text NOT NULL DEFAULT 'mixed' CONSTRAINT university_email_domains_domain_kind_chk CHECK (domain_kind IN ('student','staff','mixed')),
  is_active      boolean NOT NULL DEFAULT true,
  verified_by    uuid CONSTRAINT university_email_domains_verified_by_fkey REFERENCES users (id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX university_email_domains_uni_idx ON university_email_domains (university_id);
CREATE TRIGGER university_email_domains_touch_updated_at BEFORE UPDATE ON university_email_domains
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON university_email_domains TO app_rw;
COMMIT;
