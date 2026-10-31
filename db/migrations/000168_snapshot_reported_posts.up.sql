-- 000168_snapshot_reported_posts.up.sql (BE-160; 00 T-398, the security
-- review of 2026-10-31, G-1 continued). A report on a post keeps what was
-- reported: the post's title, plain text, opportunity and shared event as
-- they stood when the report was filed (snapshot_at). BE-158 kept the images
-- an edit takes off a published post (post_dropped_media), but updatePost
-- rewrites posts.title, body_text, opportunity and event_id in place and the
-- audit names the changed fields only, so a moderator saw the current text
-- and nothing of the reported one.
--   * Written once, with the report. A second report of the same user on
--     the same target while the first is open returns the first row
--     (content_reports_open_uq): the first snapshot stays. Every report
--     keeps the version its reporter saw; an edit adds no snapshot (the
--     current text is the post's). content_reports_snapshot_guard refuses
--     any change of the snapshot afterwards (evidence; the service never
--     writes it again).
--   * Post reports only (content_reports_snapshot_target_chk). A row without
--     snapshot_at has no snapshot (content_reports_snapshot_at_chk): an org
--     report, or a post report filed before BE-160. With snapshot_at, empty
--     columns mean the post had no title, text, opportunity or event then.
--   * The limits of posts: title 140 and body_text 5000 characters, the
--     opportunity an object. snapshot_event_id has no FK: the event may go,
--     the evidence stays.
--   * KVKK: content_reports stays personal with provider feed; the copy is
--     the reported org's content and goes with its report (the reporter's
--     erasure; content_report retention, 12 months after the resolution —
--     the seeded description below says so for the legal approval, T-35).
--     It is not in the reporter's export.
BEGIN;
SET LOCAL lock_timeout = '5s';

ALTER TABLE content_reports
  ADD COLUMN snapshot_title       text CONSTRAINT content_reports_snapshot_title_chk CHECK (char_length(snapshot_title) <= 140),
  ADD COLUMN snapshot_body_text   text CONSTRAINT content_reports_snapshot_body_text_chk CHECK (char_length(snapshot_body_text) <= 5000),
  ADD COLUMN snapshot_opportunity jsonb CONSTRAINT content_reports_snapshot_opportunity_chk CHECK (jsonb_typeof(snapshot_opportunity) = 'object'),
  ADD COLUMN snapshot_event_id    uuid,
  ADD COLUMN snapshot_at          timestamptz,
  ADD CONSTRAINT content_reports_snapshot_target_chk CHECK (snapshot_at IS NULL OR target_type = 'post'),
  ADD CONSTRAINT content_reports_snapshot_at_chk CHECK (snapshot_at IS NOT NULL
    OR (snapshot_title IS NULL AND snapshot_body_text IS NULL AND snapshot_opportunity IS NULL AND snapshot_event_id IS NULL));

CREATE FUNCTION public.content_reports_snapshot_guard() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.snapshot_title IS DISTINCT FROM OLD.snapshot_title
     OR NEW.snapshot_body_text IS DISTINCT FROM OLD.snapshot_body_text
     OR NEW.snapshot_opportunity IS DISTINCT FROM OLD.snapshot_opportunity
     OR NEW.snapshot_event_id IS DISTINCT FROM OLD.snapshot_event_id
     OR NEW.snapshot_at IS DISTINCT FROM OLD.snapshot_at THEN
    RAISE EXCEPTION 'UPDATE of content_reports.% is not allowed: the snapshot of the reported post is written with the report only', OLD.id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'content_reports_snapshot_frozen';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER content_reports_snapshot_guard
  BEFORE UPDATE OF snapshot_title, snapshot_body_text, snapshot_opportunity, snapshot_event_id, snapshot_at ON content_reports
  FOR EACH ROW EXECUTE FUNCTION public.content_reports_snapshot_guard();

-- The policy text of the reports names the copy (only the seeded,
-- unapproved row: 000161 pattern).
UPDATE public.retention_policies
   SET description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle birlikte silinir.'
 WHERE code = 'content_report' AND description_tr = 'Çözülmüş içerik şikâyetleri silinir.' AND NOT legal_approved;
COMMIT;
