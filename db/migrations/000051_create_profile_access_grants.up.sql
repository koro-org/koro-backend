-- 000051_create_profile_access_grants.up.sql (02 §9.7). Level 3 permission:
-- a company may see the profile of a student once the student accepted a
-- contact request (or, phase 3, an application). A grant is revoked, not
-- edited away; it is hard-deleted 6 months after revocation (retention
-- 'access_grant_revoked'). The company is bound to org kind 'company' by the
-- composite FK.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE profile_access_grants (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  student_user_id  uuid NOT NULL CONSTRAINT profile_access_grants_student_user_id_fkey REFERENCES users (id),
  company_org_id   uuid NOT NULL,
  org_kind         text NOT NULL DEFAULT 'company' CONSTRAINT profile_access_grants_org_kind_chk CHECK (org_kind = 'company'),
  source           text NOT NULL CONSTRAINT profile_access_grants_source_chk CHECK (source IN ('contact_request','application')),   -- application: phase 3
  source_id        uuid NOT NULL,
  granted_at       timestamptz NOT NULL DEFAULT now(),
  revoked_at       timestamptz,
  revoked_by_kind  text CONSTRAINT profile_access_grants_revoked_by_kind_chk CHECK (revoked_by_kind IN ('student','company','system')),
  revoke_reason    text CONSTRAINT profile_access_grants_revoke_reason_chk CHECK (char_length(revoke_reason) <= 300),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT profile_access_grants_company_org_id_org_kind_fkey FOREIGN KEY (company_org_id, org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT profile_access_grants_source_uq UNIQUE (source, source_id),
  CONSTRAINT profile_access_grants_revoked_chk CHECK ((revoked_at IS NULL) = (revoked_by_kind IS NULL))
);
CREATE INDEX profile_access_grants_active_idx  ON profile_access_grants (student_user_id, company_org_id) WHERE revoked_at IS NULL;
CREATE INDEX profile_access_grants_company_idx ON profile_access_grants (company_org_id, granted_at DESC) WHERE revoked_at IS NULL;
CREATE TRIGGER profile_access_grants_touch_updated_at BEFORE UPDATE ON profile_access_grants
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON profile_access_grants TO app_rw;
COMMIT;
