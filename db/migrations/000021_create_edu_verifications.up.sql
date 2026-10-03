-- 000021_create_edu_verifications.up.sql (02 §5.4). Student verification by
-- .edu.tr e-mail or by document. university_id gets its FK in 000037 (after
-- universities).
-- 06 H-37 fix (BE-011, in place before the pilot): the e-mail CHECK was
-- `(method = 'edu_email') = (edu_email IS NOT NULL AND edu_email_normalized
-- IS NOT NULL AND email_domain IS NOT NULL)`, which let a document
-- verification carry part of an e-mail address; every column is now tied to
-- the method on its own.
-- BE-018 (in place, T-220): document_method_chk lets a decided document row
-- drop its file (retention), see below.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE edu_verifications (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id               uuid NOT NULL CONSTRAINT edu_verifications_user_id_fkey REFERENCES users (id),
  method                text NOT NULL CONSTRAINT edu_verifications_method_chk CHECK (method IN ('edu_email','document')),
  edu_email             text CONSTRAINT edu_verifications_edu_email_chk CHECK (char_length(edu_email) <= 254),
  edu_email_normalized  text,
  email_domain          text,
  university_id         uuid,                 -- FK universities (000037)
  document_file_id      uuid CONSTRAINT edu_verifications_document_file_id_fkey REFERENCES files (id),   -- non-.edu.tr student document (admin approved)
  status                text NOT NULL DEFAULT 'pending'
                          CONSTRAINT edu_verifications_status_chk CHECK (status IN ('pending','verified','rejected','expired','revoked','superseded')),
  verified_at           timestamptz,
  expires_at            timestamptz,          -- verified_at + 1 year
  reviewed_by           uuid CONSTRAINT edu_verifications_reviewed_by_fkey REFERENCES users (id),
  review_note           text CONSTRAINT edu_verifications_review_note_chk CHECK (char_length(review_note) <= 500),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT edu_verifications_email_method_chk CHECK ((method = 'edu_email') = (edu_email IS NOT NULL)
                                                        AND (method = 'edu_email') = (edu_email_normalized IS NOT NULL)
                                                        AND (method = 'edu_email') = (email_domain IS NOT NULL)),
  -- BE-018 (in place before the pilot): a decided document verification
  -- may lose its file (edu_document retention: decision + 30 days, 02 §24.2;
  -- a virus finding, 01 §6.2 file.rejected); a pending one always has it,
  -- an e-mail verification never.
  CONSTRAINT edu_verifications_document_method_chk CHECK (CASE WHEN method = 'document'
                                                               THEN status <> 'pending' OR document_file_id IS NOT NULL
                                                               ELSE document_file_id IS NULL END),
  CONSTRAINT edu_verifications_verified_chk CHECK (status <> 'verified' OR (verified_at IS NOT NULL AND expires_at IS NOT NULL AND university_id IS NOT NULL)),
  CONSTRAINT edu_verifications_expiry_chk CHECK (expires_at IS NULL OR expires_at > verified_at),
  CONSTRAINT edu_verifications_document_review_chk CHECK (method <> 'document' OR status NOT IN ('verified','rejected') OR reviewed_by IS NOT NULL),
  CONSTRAINT edu_verifications_reviewer_chk CHECK (reviewed_by IS NULL OR reviewed_by <> user_id)   -- nobody verifies their own document (03 Y6)
);
CREATE UNIQUE INDEX edu_verifications_one_verified_uq ON edu_verifications (user_id) WHERE status = 'verified';
CREATE UNIQUE INDEX edu_verifications_one_pending_uq  ON edu_verifications (user_id) WHERE status = 'pending';
CREATE UNIQUE INDEX edu_verifications_email_uq        ON edu_verifications (edu_email_normalized)
  WHERE status IN ('pending','verified');                      -- one .edu.tr address verifies one account
CREATE INDEX edu_verifications_expiry_idx ON edu_verifications (expires_at) WHERE status = 'verified';
CREATE TRIGGER edu_verifications_touch_updated_at BEFORE UPDATE ON edu_verifications
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON edu_verifications TO app_rw;
COMMIT;
