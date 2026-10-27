-- 000159_organizations_rejection_reason.up.sql (BE-144; 02 §7.1 doc patch).
-- A pending_review org that platform staff close is rejected: the founder
-- is told why (orgs.notify_review_decided e-mail). The reason is the
-- closeOrg body's (3–500 characters, trimmed); the event never carries it
-- (01 §5.4), so the worker reads it here. Only a closed org carries one:
-- an owner's own close (withdrawal) and every other close leave it NULL.
-- One CHECK per rule (06 H-37: no "(state) = (A AND B)").
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE organizations ADD COLUMN rejection_reason text;
ALTER TABLE organizations ADD CONSTRAINT organizations_rejection_reason_chk CHECK (
  rejection_reason IS NULL OR (status = 'closed' AND char_length(rejection_reason) BETWEEN 3 AND 500));
COMMIT;
