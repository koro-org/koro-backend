-- 000042_create_club_claims.up.sql (02 §8.6). A student's claim to run an
-- unclaimed club; decided by a platform admin who is never the claimant.
-- 06 H-37 fix (BE-011, in place before the pilot): the review CHECK was
-- `(status IN ('approved','rejected')) = (reviewed_by IS NOT NULL AND
-- reviewed_at IS NOT NULL)`, which let a pending or withdrawn claim carry
-- half a review (only the reviewer or only the time); each column is now
-- tied to the decided state on its own.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE club_claims (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  club_org_id       uuid NOT NULL CONSTRAINT club_claims_club_org_id_fkey REFERENCES clubs (org_id),
  claimant_user_id  uuid NOT NULL CONSTRAINT club_claims_claimant_user_id_fkey REFERENCES users (id),
  status            text NOT NULL DEFAULT 'pending' CONSTRAINT club_claims_status_chk CHECK (status IN ('pending','approved','rejected','withdrawn')),
  role_in_club      text NOT NULL CONSTRAINT club_claims_role_in_club_chk CHECK (char_length(role_in_club) BETWEEN 2 AND 100),   -- "Başkan", "YK üyesi"
  evidence_text     text CONSTRAINT club_claims_evidence_text_chk CHECK (char_length(evidence_text) <= 2000),
  evidence_file_id  uuid CONSTRAINT club_claims_evidence_file_id_fkey REFERENCES files (id),
  reviewed_by       uuid CONSTRAINT club_claims_reviewed_by_fkey REFERENCES users (id),               -- platform admin
  reviewed_at       timestamptz,
  decision_note     text CONSTRAINT club_claims_decision_note_chk CHECK (char_length(decision_note) <= 1000),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT club_claims_review_chk CHECK ((status IN ('approved','rejected')) = (reviewed_by IS NOT NULL)
                                         AND (status IN ('approved','rejected')) = (reviewed_at IS NOT NULL)),
  CONSTRAINT club_claims_reviewer_chk CHECK (reviewed_by IS NULL OR reviewed_by <> claimant_user_id)   -- nobody decides their own claim (03 Y6)
);
CREATE UNIQUE INDEX club_claims_pending_uq ON club_claims (club_org_id, claimant_user_id) WHERE status = 'pending';
CREATE INDEX club_claims_queue_idx ON club_claims (created_at) WHERE status = 'pending';
CREATE TRIGGER club_claims_touch_updated_at BEFORE UPDATE ON club_claims
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON club_claims TO app_rw;
COMMIT;
