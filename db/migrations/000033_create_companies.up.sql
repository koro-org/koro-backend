-- 000033_create_companies.up.sql (02 §7.2). Company-specific data of an org
-- of kind 'company' (1:1). Invoice title and tax number live in
-- parties.legal_parties, not here.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE companies (
  org_id              uuid PRIMARY KEY,
  org_kind            text NOT NULL DEFAULT 'company' CONSTRAINT companies_org_kind_chk CHECK (org_kind = 'company'),
  sector_code         text CONSTRAINT companies_sector_code_chk CHECK (sector_code ~ '^[a-z_]{2,40}$'),
  size_band           text CONSTRAINT companies_size_band_chk CHECK (size_band IN ('1-10','11-50','51-200','201-1000','1000+')),
  email_domain        text CONSTRAINT companies_email_domain_chk CHECK (email_domain ~ '^[a-z0-9.-]+\.[a-z]{2,}$'),
  domain_verified_at  timestamptz,
  hq_city             text COLLATE public.tr,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT companies_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind)
);
CREATE TRIGGER companies_touch_updated_at BEFORE UPDATE ON companies
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON companies TO app_rw;
COMMIT;
