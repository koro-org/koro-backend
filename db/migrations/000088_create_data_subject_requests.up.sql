-- 000088_create_data_subject_requests.up.sql (02 §16.1). A data subject
-- request (KVKK md. 11, answered within 30 days, md. 13); registered users
-- or applicants entered by the DPO. A profiling-access answer needs a second
-- approver who is not the first. Kept 3 years after closing (dsr_record),
-- also after the account is erased (proof).
-- 02 fixes (H-20):
--   * data_subject_requests_approvers_chk: `second_approver <> first_approver`
--     is NULL when there is no first approver, which let a second approval
--     stand alone; a second approver now needs the first.
--   * index names carry the table name (02 §23): dsr_open_idx →
--     data_subject_requests_open_idx, dsr_user_idx → data_subject_requests_user_idx.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE data_subject_requests (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id               uuid CONSTRAINT data_subject_requests_user_id_fkey REFERENCES users (id),   -- NULL: unregistered applicant (entered by the DPO)
  requester_name        text COLLATE "tr" CONSTRAINT data_subject_requests_requester_name_chk CHECK (char_length(requester_name) <= 200),   -- only when user_id is NULL
  requester_email       text CONSTRAINT data_subject_requests_requester_email_chk CHECK (char_length(requester_email) <= 254),
  type                  text NOT NULL CONSTRAINT data_subject_requests_type_chk CHECK (type IN ('access','export','rectification','erasure','restriction',
                          'objection','profiling_access','profiling_objection','info')),
  channel               text NOT NULL CONSTRAINT data_subject_requests_channel_chk CHECK (channel IN ('in_app','email','post','kep','in_person')),
  status                text NOT NULL DEFAULT 'received' CONSTRAINT data_subject_requests_status_chk CHECK (status IN ('received','identity_check','in_progress',
                          'awaiting_second_approval','completed','rejected','cancelled')),
  details               text CONSTRAINT data_subject_requests_details_chk CHECK (char_length(details) <= 4000),
  received_at           timestamptz NOT NULL DEFAULT now(),
  due_at                timestamptz NOT NULL,                  -- received_at + 30 days (KVKK md. 13)
  identity_verified_at  timestamptz,
  assigned_to           uuid CONSTRAINT data_subject_requests_assigned_to_fkey REFERENCES users (id),       -- dpo
  first_approver        uuid CONSTRAINT data_subject_requests_first_approver_fkey REFERENCES users (id),
  second_approver       uuid CONSTRAINT data_subject_requests_second_approver_fkey REFERENCES users (id),
  response_summary      text CONSTRAINT data_subject_requests_response_summary_chk CHECK (char_length(response_summary) <= 4000),
  export_file_id        uuid CONSTRAINT data_subject_requests_export_file_id_fkey REFERENCES files (id),
  rejection_reason      text CONSTRAINT data_subject_requests_rejection_reason_chk CHECK (char_length(rejection_reason) <= 1000),
  completed_at          timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT data_subject_requests_requester_chk CHECK (user_id IS NOT NULL OR requester_email IS NOT NULL),
  CONSTRAINT data_subject_requests_approvers_chk CHECK (second_approver IS NULL OR (first_approver IS NOT NULL AND second_approver <> first_approver)),
  CONSTRAINT data_subject_requests_profiling_approval_chk CHECK (type <> 'profiling_access' OR status <> 'completed' OR second_approver IS NOT NULL),
  CONSTRAINT data_subject_requests_completed_chk CHECK ((status IN ('completed','rejected')) = (completed_at IS NOT NULL)),
  CONSTRAINT data_subject_requests_rejected_chk CHECK (status <> 'rejected' OR rejection_reason IS NOT NULL)
);
CREATE INDEX data_subject_requests_open_idx ON data_subject_requests (due_at) WHERE status NOT IN ('completed','rejected','cancelled');
CREATE INDEX data_subject_requests_user_idx ON data_subject_requests (user_id, received_at DESC);
CREATE TRIGGER data_subject_requests_touch_updated_at BEFORE UPDATE ON data_subject_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON data_subject_requests TO app_rw;
COMMIT;
