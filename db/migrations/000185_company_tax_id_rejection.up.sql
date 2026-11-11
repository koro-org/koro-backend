-- 000185_company_tax_id_rejection.up.sql (BE-208; user decision Q-63, T-426). A
-- platform admin's rejection of a company's new tax identifier (BE-206: it
-- suspends the company) is explained to the company: its editors read the reason
-- in the panel (OrgDTO.company) and in the rejection e-mail. The event never
-- carries it (01 §5.4: events hold no free text), so the worker reads it here.
-- * tax_id_rejected_at / tax_id_rejection_reason: the rejection's time and the
--   staff's reason (3–500 characters, trimmed; the reject body's). Both or
--   neither: one CHECK per direction (06 H-37: no "(state) = (A AND B)").
-- * A rejection and a pending review never coexist: the rejection ends the
--   review, and a new tax identifier clears the rejection before it flags one.
-- * Cleared when the company is reactivated, when its tax identifier is
--   corrected (a platform admin's updateOrg) and when it closes: the reason
--   lives as long as the suspension it explains. The audit record of the
--   rejection keeps it (audit_events retention).
-- * KVKK: staff text about a company, not about a person; the admin screen
--   tells the staff not to write personal data in it. companies stays class
--   none (personal_tables.txt); no copy of a number is kept.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE companies
  ADD COLUMN tax_id_rejected_at timestamptz,
  ADD COLUMN tax_id_rejection_reason text;
ALTER TABLE companies
  ADD CONSTRAINT companies_tax_id_rejection_reason_chk CHECK (
    tax_id_rejection_reason IS NULL OR char_length(tax_id_rejection_reason) BETWEEN 3 AND 500),
  ADD CONSTRAINT companies_tax_id_rejection_reason_at_chk CHECK (
    tax_id_rejection_reason IS NULL OR tax_id_rejected_at IS NOT NULL),
  ADD CONSTRAINT companies_tax_id_rejected_at_reason_chk CHECK (
    tax_id_rejected_at IS NULL OR tax_id_rejection_reason IS NOT NULL),
  ADD CONSTRAINT companies_tax_id_rejection_review_chk CHECK (
    tax_id_rejected_at IS NULL OR tax_id_review_requested_at IS NULL);
COMMIT;
