-- 000173_company_tax_id.down.sql
-- DATA LOSS: the tax identity of company applications (kind, VKN, tax office).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE companies
  DROP CONSTRAINT companies_tax_no_kind_chk,
  DROP CONSTRAINT companies_tax_vkn_chk,
  DROP COLUMN tax_office,
  DROP COLUMN tax_no,
  DROP COLUMN tax_id_kind;
COMMIT;
