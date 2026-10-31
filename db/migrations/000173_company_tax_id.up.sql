-- 000173_company_tax_id.up.sql (BE-165; Q-33). The tax identity a company
-- gives when it applies (createCompany) or edits its profile (updateOrg),
-- for the platform admin's review: a company registers itself, an admin
-- verifies it.
--   * tax_id_kind: which identifier the company gave: 'vkn' (vergi kimlik
--     numarası, a legal entity or a sole trader's VKN) or 'tckn' (a sole
--     trader's T.C. kimlik numarası). The service checks the check digits
--     before the write (platform/taxid).
--   * tax_no: the VKN, 10 digits; only with tax_id_kind 'vkn'. A TCKN is
--     personal data and is not kept here (06 H-60 data minimisation): the
--     row keeps only that a valid one was given (tax_id_kind 'tckn'); the
--     legal party of invoicing (parties, legal_parties.tckn_enc) holds it
--     encrypted when it is needed.
--   * tax_office: the vergi dairesi, 1–100 characters (legal_parties.tax_office).
-- The columns stay nullable: the tax identity is required by the service
-- (createCompany; updateOrg never clears it, Q-45), not by the schema, so
-- that OrgAdmin.Create (an empty company profile) and the companies from
-- before BE-165 keep working.
-- The invoice title, address and MERSİS number stay in legal_parties
-- (02 §7.2); domain_verified_at (000033) is now written: the founder's or
-- an editor's verified address is at email_domain.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE companies
  ADD COLUMN tax_id_kind text CONSTRAINT companies_tax_id_kind_chk CHECK (tax_id_kind IN ('vkn','tckn')),
  ADD COLUMN tax_no      text CONSTRAINT companies_tax_no_chk CHECK (tax_no ~ '^[0-9]{10}$'),
  ADD COLUMN tax_office  text COLLATE public.tr
    CONSTRAINT companies_tax_office_chk CHECK (char_length(tax_office) BETWEEN 1 AND 100 AND tax_office = btrim(tax_office)),
  ADD CONSTRAINT companies_tax_vkn_chk CHECK (tax_id_kind IS DISTINCT FROM 'vkn' OR tax_no IS NOT NULL),
  ADD CONSTRAINT companies_tax_no_kind_chk CHECK (tax_no IS NULL OR tax_id_kind IS NOT DISTINCT FROM 'vkn');
COMMIT;
