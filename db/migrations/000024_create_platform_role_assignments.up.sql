-- 000024_create_platform_role_assignments.up.sql (02 §5.7; 06 H-17, H-20).
-- Platform role assignments with FOUR EYES: granting or revoking
-- platform_admin, finance_admin or talent_auditor needs a second
-- platform_admin. A grant is a row that starts 'pending' (requested_by) and
-- becomes 'active' when a different admin approves it (approved_by); a
-- revocation of such a role goes 'revoke_pending' (revoke_requested_by) →
-- 'revoked' (revoked_by) the same way. moderator, support and dpo are
-- granted and revoked by one admin directly (no approver). The CHECKs are
-- the second layer next to the authz four-eyes rule (03 §6.1).
--
-- A role is IN EFFECT while status IN ('active','revoke_pending'): readers of
-- the roles of a user must filter on status, never on revoked_at alone (a
-- 'pending' row has revoked_at NULL too).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE platform_role_assignments (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id              uuid NOT NULL CONSTRAINT platform_role_assignments_user_id_fkey REFERENCES users (id),   -- who gets / holds the role
  role                 text NOT NULL CONSTRAINT platform_role_assignments_role_chk CHECK (role IN ('platform_admin','moderator','finance_admin','talent_auditor','support','dpo')),
  reason               text NOT NULL CONSTRAINT platform_role_assignments_reason_chk CHECK (char_length(reason) BETWEEN 5 AND 500),
  status               text NOT NULL DEFAULT 'pending'
                         CONSTRAINT platform_role_assignments_status_chk CHECK (status IN ('pending','active','revoke_pending','revoked','rejected')),
  -- grant: request → approval
  requested_by         uuid CONSTRAINT platform_role_assignments_requested_by_fkey REFERENCES users (id),      -- NULL only for `koro seed bootstrap-admin` (reason 'bootstrap')
  requested_at         timestamptz NOT NULL DEFAULT now(),
  approved_by          uuid CONSTRAINT platform_role_assignments_approved_by_fkey REFERENCES users (id),       -- second eye (H-17); NULL for moderator/support/dpo and bootstrap
  approved_at          timestamptz,
  granted_at           timestamptz,                                                                          -- role in effect since
  rejected_by          uuid CONSTRAINT platform_role_assignments_rejected_by_fkey REFERENCES users (id),       -- a pending request refused or withdrawn
  rejected_at          timestamptz,
  -- revocation: request → approval (privileged roles); moderator/support/dpo go straight to 'revoked'
  revoke_reason        text CONSTRAINT platform_role_assignments_revoke_reason_chk CHECK (char_length(revoke_reason) BETWEEN 5 AND 500),
  revoke_requested_by  uuid CONSTRAINT platform_role_assignments_revoke_requested_by_fkey REFERENCES users (id),
  revoke_requested_at  timestamptz,
  revoked_by           uuid CONSTRAINT platform_role_assignments_revoked_by_fkey REFERENCES users (id),        -- the approver of a privileged revocation
  revoked_at           timestamptz,
  -- nobody grants, approves or revokes their own role (03 Y6)
  CONSTRAINT platform_role_assignments_self_chk CHECK (requested_by <> user_id AND approved_by <> user_id
         AND revoke_requested_by <> user_id AND revoked_by <> user_id),
  CONSTRAINT platform_role_assignments_bootstrap_chk CHECK (requested_by IS NOT NULL OR reason = 'bootstrap'),
  -- FOUR EYES (H-17): the approver is not the requester
  CONSTRAINT platform_role_assignments_four_eyes_chk CHECK (approved_by <> requested_by),
  CONSTRAINT platform_role_assignments_revoke_four_eyes_chk CHECK (role NOT IN ('platform_admin','finance_admin','talent_auditor')
         OR status <> 'revoked' OR (revoke_requested_by IS NOT NULL AND revoked_by <> revoke_requested_by)),
  -- a privileged role is never in effect without a second approver (the bootstrap row has no requester at all)
  CONSTRAINT platform_role_assignments_privileged_chk CHECK (role NOT IN ('platform_admin','finance_admin','talent_auditor')
         OR status IN ('pending','rejected') OR approved_by IS NOT NULL OR requested_by IS NULL),
  CONSTRAINT platform_role_assignments_approval_chk CHECK ((approved_by IS NULL) = (approved_at IS NULL)
         AND (approved_by IS NULL OR status IN ('active','revoke_pending','revoked'))),
  CONSTRAINT platform_role_assignments_granted_chk CHECK ((status IN ('active','revoke_pending','revoked')) = (granted_at IS NOT NULL)),
  CONSTRAINT platform_role_assignments_rejected_chk CHECK ((rejected_by IS NULL) = (rejected_at IS NULL)
         AND (status = 'rejected') = (rejected_at IS NOT NULL)),
  CONSTRAINT platform_role_assignments_revoke_request_chk CHECK ((revoke_requested_by IS NULL) = (revoke_requested_at IS NULL)
         AND (status <> 'revoke_pending' OR revoke_requested_by IS NOT NULL)
         AND (revoke_requested_by IS NULL OR status IN ('revoke_pending','revoked'))),
  CONSTRAINT platform_role_assignments_revoked_chk CHECK ((revoked_by IS NULL) = (revoked_at IS NULL)
         AND (status = 'revoked') = (revoked_at IS NOT NULL)
         AND (status IN ('revoke_pending','revoked')) = (revoke_reason IS NOT NULL))
);
-- one open assignment (awaiting approval, in effect or awaiting revocation) per user and role
CREATE UNIQUE INDEX platform_role_assignments_open_uq ON platform_role_assignments (user_id, role)
  WHERE status IN ('pending','active','revoke_pending');
GRANT SELECT, INSERT, UPDATE, DELETE ON platform_role_assignments TO app_rw;
COMMIT;
