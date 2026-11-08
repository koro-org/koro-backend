-- 000183_company_tax_id_review.up.sql (BE-206; user decision Q-55, T-415). A
-- member's new tax identifier of an active company no longer sends it back to
-- review (BE-183 did: pending_review); the company stays active and the change
-- waits in the platform admins' queue.
-- * companies.tax_id_review_requested_at: a member of the company replaced its
--   tax identifier at this time (the first replacement while none was pending)
--   and no platform admin has decided yet; NULL: nothing to review. Approving
--   the change clears it; rejecting it clears it and suspends the company.
-- * KVKK: a time only. Neither the previous nor the new identifier is copied
--   anywhere (a TCKN is never stored, 06 H-60); the change is in the audit trail
--   without the numbers. companies holds no personal data.
-- * No index: companies is small and the queue (adminListOrgs?tax_id_review,
--   the queue counter) reads it through the org list.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE companies ADD COLUMN tax_id_review_requested_at timestamptz;
COMMIT;
