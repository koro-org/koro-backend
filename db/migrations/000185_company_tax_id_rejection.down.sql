-- 000185_company_tax_id_rejection.down.sql (BE-208).
-- DATA LOSS: the reasons of the tax identifier rejections still in force (the
-- audit trail keeps them). Roll back only together with the code that uses them.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE companies
  DROP COLUMN tax_id_rejection_reason,
  DROP COLUMN tax_id_rejected_at;
COMMIT;
