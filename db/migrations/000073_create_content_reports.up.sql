-- 000073_create_content_reports.up.sql (02 §12.9). A user's report of a post
-- or an org, handled by platform moderation. The author org never learns
-- who reported. One open report per reporter and target
-- (content_reports_open_uq). Hard-deleted 12 months after resolution.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE content_reports (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  reporter_user_id  uuid NOT NULL CONSTRAINT content_reports_reporter_user_id_fkey REFERENCES users (id),
  target_type       text NOT NULL CONSTRAINT content_reports_target_type_chk CHECK (target_type IN ('post','org')),
  target_id         uuid NOT NULL,                                   -- posts.id or organizations.id (no FK: polymorphic)
  reason            text NOT NULL CONSTRAINT content_reports_reason_chk CHECK (reason IN ('spam','harassment','hate','sexual','scam_or_misleading_job',
                                                    'copyright','privacy','other')),
  details           text CONSTRAINT content_reports_details_chk CHECK (char_length(details) <= 1000),
  status            text NOT NULL DEFAULT 'open' CONSTRAINT content_reports_status_chk CHECK (status IN ('open','in_review','actioned','dismissed')),
  assigned_to       uuid CONSTRAINT content_reports_assigned_to_fkey REFERENCES users (id),
  resolution_note   text CONSTRAINT content_reports_resolution_note_chk CHECK (char_length(resolution_note) <= 1000),
  resolved_at       timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT content_reports_resolved_chk CHECK ((status IN ('actioned','dismissed')) = (resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX content_reports_open_uq ON content_reports (reporter_user_id, target_type, target_id)
  WHERE status IN ('open','in_review');
CREATE INDEX content_reports_queue_idx ON content_reports (created_at) WHERE status IN ('open','in_review');
CREATE TRIGGER content_reports_touch_updated_at BEFORE UPDATE ON content_reports
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON content_reports TO app_rw;
COMMIT;
