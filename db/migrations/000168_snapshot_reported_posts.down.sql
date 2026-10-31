-- 000168_snapshot_reported_posts.down.sql
-- DATA LOSS: the snapshots of the reported posts (title, text, opportunity,
-- shared event); a moderator sees the current post only again. Roll back
-- only together with the code.
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE public.retention_policies
   SET description_tr = 'Çözülmüş içerik şikâyetleri silinir.'
 WHERE code = 'content_report' AND description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle birlikte silinir.'
   AND NOT legal_approved;
DROP TRIGGER content_reports_snapshot_guard ON content_reports;
DROP FUNCTION public.content_reports_snapshot_guard();
ALTER TABLE content_reports
  DROP CONSTRAINT content_reports_snapshot_at_chk,
  DROP CONSTRAINT content_reports_snapshot_target_chk,
  DROP COLUMN snapshot_at,
  DROP COLUMN snapshot_event_id,
  DROP COLUMN snapshot_opportunity,
  DROP COLUMN snapshot_body_text,
  DROP COLUMN snapshot_title;
COMMIT;
