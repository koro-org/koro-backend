-- 000034_create_org_members.up.sql (02 §7.3). Memberships. A role change is
-- an 'ended' row (role_changed) plus a new row, so history is kept. Sensitive
-- club memberships are special-category data (02 §7.3 rules): they end by
-- hard delete right away. Business rules the schema enforces: one open
-- membership per user and org, exactly one active company_owner per org, the
-- role fits the org kind.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE org_members (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id        uuid NOT NULL,
  org_kind      text NOT NULL,
  user_id       uuid NOT NULL CONSTRAINT org_members_user_id_fkey REFERENCES users (id),
  role          text NOT NULL,
  status        text NOT NULL CONSTRAINT org_members_status_chk CHECK (status IN ('pending','active','ended')),
  source        text NOT NULL CONSTRAINT org_members_source_chk CHECK (source IN ('join_request','invitation','claim','admin','import')),
  requested_at  timestamptz,
  joined_at     timestamptz,
  ended_at      timestamptz,
  end_reason    text CONSTRAINT org_members_end_reason_chk CHECK (end_reason IN ('left','removed','rejected','graduated','verification_lapsed',
                                          'claim_revoked','account_erased','org_closed','role_changed')),
  invited_by    uuid CONSTRAINT org_members_invited_by_fkey REFERENCES users (id),
  approved_by   uuid CONSTRAINT org_members_approved_by_fkey REFERENCES users (id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_members_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT org_members_role_kind_chk CHECK ( (org_kind = 'club'       AND role IN ('member','club_admin'))
       OR (org_kind = 'university' AND role IN ('university_rep','career_staff'))
       OR (org_kind = 'company'    AND role IN ('company_owner','company_admin','recruiter'))
       OR (org_kind = 'platform'   AND role IN ('staff'))
       OR (org_kind = 'ngo'        AND role IN ('ngo_admin','ngo_member')) ),
  CONSTRAINT org_members_active_chk  CHECK ((status = 'active') = (joined_at IS NOT NULL AND ended_at IS NULL)),
  CONSTRAINT org_members_ended_chk   CHECK ((status = 'ended')  = (ended_at IS NOT NULL AND end_reason IS NOT NULL)),
  CONSTRAINT org_members_pending_chk CHECK (status <> 'pending' OR requested_at IS NOT NULL)
);
CREATE UNIQUE INDEX org_members_open_uq      ON org_members (org_id, user_id) WHERE status IN ('pending','active');
CREATE UNIQUE INDEX org_members_one_owner_uq ON org_members (org_id) WHERE role = 'company_owner' AND status = 'active';
CREATE INDEX org_members_user_idx     ON org_members (user_id) WHERE status = 'active';
CREATE INDEX org_members_org_role_idx ON org_members (org_id, role) WHERE status = 'active';
CREATE INDEX org_members_ended_idx    ON org_members (ended_at) WHERE status = 'ended';
CREATE TRIGGER org_members_touch_updated_at BEFORE UPDATE ON org_members
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON org_members TO app_rw;
COMMIT;
