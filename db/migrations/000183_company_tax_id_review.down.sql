-- 000183_company_tax_id_review.down.sql (BE-206).
-- DATA LOSS: the pending tax identifier reviews of active companies (the audit
-- trail keeps the changes). Roll back only together with the code that uses it.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE companies DROP COLUMN tax_id_review_requested_at;
COMMIT;
