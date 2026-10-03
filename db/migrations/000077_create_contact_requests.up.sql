-- 000077_create_contact_requests.up.sql (02 §14.1). Schema only (the
-- messaging code is phase 3): a company's contact request to a student,
-- paid with a contact_request entitlement. At most one pending request per
-- company and student (contact_requests_pending_uq); the company is bound to
-- org kind 'company' by the composite FK. Hard-deleted 12 months after it
-- is closed (contact_request_closed).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE contact_requests (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id                uuid NOT NULL,                                   -- the requesting company
  org_kind              text NOT NULL DEFAULT 'company' CONSTRAINT contact_requests_org_kind_chk CHECK (org_kind = 'company'),
  sender_user_id        uuid NOT NULL CONSTRAINT contact_requests_sender_user_id_fkey REFERENCES users (id),
  student_user_id       uuid NOT NULL CONSTRAINT contact_requests_student_user_id_fkey REFERENCES users (id),
  via                   text NOT NULL CONSTRAINT contact_requests_via_chk CHECK (via IN ('discovery','opportunity_post')),
  opportunity_post_id   uuid CONSTRAINT contact_requests_opportunity_post_id_fkey REFERENCES posts (id),
  subject               text NOT NULL CONSTRAINT contact_requests_subject_chk CHECK (char_length(subject) BETWEEN 3 AND 120),
  message               text NOT NULL CONSTRAINT contact_requests_message_chk CHECK (char_length(message) BETWEEN 10 AND 1000),
  status                text NOT NULL DEFAULT 'pending'
                          CONSTRAINT contact_requests_status_chk CHECK (status IN ('pending','accepted','declined','expired','withdrawn')),
  responded_at          timestamptz,
  expires_at            timestamptz NOT NULL,                            -- created_at + 14 days
  entitlement_usage_id  uuid NOT NULL CONSTRAINT contact_requests_entitlement_usage_id_fkey REFERENCES entitlement_usages (id),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_requests_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT contact_requests_opportunity_post_chk CHECK ((via = 'opportunity_post') = (opportunity_post_id IS NOT NULL)),
  CONSTRAINT contact_requests_responded_chk CHECK ((status IN ('accepted','declined')) = (responded_at IS NOT NULL))
);
CREATE UNIQUE INDEX contact_requests_pending_uq ON contact_requests (org_id, student_user_id) WHERE status = 'pending';
CREATE INDEX contact_requests_student_idx ON contact_requests (student_user_id, status, created_at DESC);
CREATE INDEX contact_requests_org_idx     ON contact_requests (org_id, created_at DESC);
CREATE INDEX contact_requests_expiry_idx  ON contact_requests (expires_at) WHERE status = 'pending';
CREATE TRIGGER contact_requests_touch_updated_at BEFORE UPDATE ON contact_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON contact_requests TO app_rw;
COMMIT;
