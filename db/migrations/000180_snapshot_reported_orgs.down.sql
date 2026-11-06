-- 000180_snapshot_reported_orgs.down.sql
-- DATA LOSS: the snapshots of the reported orgs and the rows of the kept
-- logos (their files stay attached to their orgs; the code before BE-180
-- neither shows nor purges them, and files.publish_public copies them to
-- public media again). Roll back only together with the code.
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE public.retention_policies
   SET description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle ve mesaj şikâyetinde saklanan mesaj kopyasıyla (şikâyet edilen mesaj ve öncesindeki en çok 5 mesaj) birlikte silinir.'
 WHERE code = 'content_report' AND description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle ya da kuruluş profiliyle ve mesaj şikâyetinde saklanan mesaj kopyasıyla (şikâyet edilen mesaj ve öncesindeki en çok 5 mesaj) birlikte silinir.'
   AND NOT legal_approved;
-- The policy goes unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then).
DELETE FROM public.retention_policies WHERE code = 'org_logo_dropped'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'org_logo_dropped');

ALTER TABLE files DROP CONSTRAINT files_retired_public_chk;
ALTER TABLE files DROP COLUMN retired_at;

DROP TABLE org_dropped_logos;

-- The guard goes first: an org report loses its snapshot here (000168's
-- content_reports_snapshot_target_chk allows a post report's only).
DROP TRIGGER content_reports_snapshot_guard ON content_reports;
UPDATE content_reports SET snapshot_at = NULL, snapshot_org_name = NULL, snapshot_org_description = NULL,
    snapshot_org_website_url = NULL, snapshot_org_social_links = NULL, snapshot_org_logo_file_id = NULL
  WHERE target_type = 'org' AND snapshot_at IS NOT NULL;
ALTER TABLE content_reports
  DROP CONSTRAINT content_reports_snapshot_org_name_required_chk,
  DROP CONSTRAINT content_reports_snapshot_org_columns_chk,
  DROP CONSTRAINT content_reports_snapshot_post_columns_chk,
  DROP CONSTRAINT content_reports_snapshot_at_chk,
  DROP CONSTRAINT content_reports_snapshot_target_chk,
  DROP COLUMN snapshot_org_logo_file_id,
  DROP COLUMN snapshot_org_social_links,
  DROP COLUMN snapshot_org_website_url,
  DROP COLUMN snapshot_org_description,
  DROP COLUMN snapshot_org_name,
  ADD CONSTRAINT content_reports_snapshot_target_chk CHECK (snapshot_at IS NULL OR target_type = 'post'),
  ADD CONSTRAINT content_reports_snapshot_at_chk CHECK (snapshot_at IS NOT NULL
    OR (snapshot_title IS NULL AND snapshot_body_text IS NULL AND snapshot_opportunity IS NULL AND snapshot_event_id IS NULL));

CREATE OR REPLACE FUNCTION public.content_reports_snapshot_guard() RETURNS trigger
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
COMMIT;
