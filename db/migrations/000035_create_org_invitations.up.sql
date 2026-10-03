-- 000035_create_org_invitations.up.sql (02 §7.4). Invitations to an org by
-- e-mail; only the SHA-256 of the derived token is stored. company_owner and
-- the platform/ngo roles cannot be invited.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE org_invitations (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id            uuid NOT NULL,
  org_kind          text NOT NULL,
  email             text NOT NULL CONSTRAINT org_invitations_email_chk CHECK (char_length(email) <= 254),
  email_normalized  text NOT NULL,
  role              text NOT NULL,                        -- same role set as org_members
  key_id            text NOT NULL,
  token_hash        bytea NOT NULL CONSTRAINT org_invitations_token_hash_uq UNIQUE CONSTRAINT org_invitations_token_hash_chk CHECK (octet_length(token_hash) = 32),
  invited_by        uuid NOT NULL CONSTRAINT org_invitations_invited_by_fkey REFERENCES users (id),
  expires_at        timestamptz NOT NULL,                 -- created_at + 7 days
  accepted_at       timestamptz,
  accepted_user_id  uuid CONSTRAINT org_invitations_accepted_user_id_fkey REFERENCES users (id),
  revoked_at        timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_invitations_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT org_invitations_role_kind_chk CHECK ( (org_kind = 'club'       AND role IN ('member','club_admin'))
       OR (org_kind = 'university' AND role IN ('university_rep','career_staff'))
       OR (org_kind = 'company'    AND role IN ('company_admin','recruiter')) ),
  CONSTRAINT org_invitations_accepted_chk CHECK ((accepted_at IS NULL) = (accepted_user_id IS NULL)),
  CONSTRAINT org_invitations_outcome_chk CHECK (NOT (accepted_at IS NOT NULL AND revoked_at IS NOT NULL))
);
CREATE UNIQUE INDEX org_invitations_open_uq ON org_invitations (org_id, email_normalized)
  WHERE accepted_at IS NULL AND revoked_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON org_invitations TO app_rw;
COMMIT;
