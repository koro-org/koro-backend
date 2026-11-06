-- 000180_snapshot_reported_orgs.up.sql (BE-180; 00 T-401, BE-160 continued).
-- A report on an org keeps what was reported, as a report on a post does
-- (000168): updateOrg rewrites organizations.name, description, website_url
-- and logo_file_id in place and deleted the replaced logo at once (BE-155),
-- directory rewrites clubs.social_links, and the audit names the changed
-- fields only, so a profile could be changed before a moderator looked.
--   * content_reports.snapshot_org_*: the org's name, description, website,
--     social links (a club's) and logo file as they stood when the report
--     was filed (snapshot_at). Written once, with the report: the same rules
--     and the same guard as the post columns (content_reports_snapshot_guard
--     now covers them too). A second report of the same user while the first
--     is open returns the first row: the first snapshot stays; an edit adds
--     none (BE-160: every report keeps the version its reporter saw). An org
--     report filed before BE-180 keeps none: the current profile is not the
--     reported one. snapshot_org_logo_file_id has no FK: the file may go, the
--     evidence of which file it was stays.
--   * org_dropped_logos: a logo updateOrg replaces or removes while a report
--     on the org is open or in review is not deleted. Its file stays attached
--     to the org behind this row, shown to the moderators only
--     (adminGetReport). feed owns the row (orgs asks through a port): the
--     rule depends on the reports. The row goes with its file (ON DELETE
--     CASCADE: the scan may still reject it, orgs.drop_rejected_logo) or
--     through the org_logo_dropped policy below. No user column: the editor
--     is in the audit record (orgs.org.update).
--   * files.retired_at: the kept logo is no longer public. Files.Retire takes
--     its public media copy down in the same transaction (BE-155 promises
--     that of a replaced logo), files.publish_public never copies it again
--     and nobody attaches it again: to the org it is gone, as a deleted file.
--   * KVKK: both are the org's content, not the reporter's data. The copy
--     goes with its report (the reporter's erasure, content_report retention)
--     and is not in the reporter's export; org_dropped_logos has no personal
--     column and its own policy (unapproved like every seeded row, T-35).
BEGIN;
SET LOCAL lock_timeout = '5s';

ALTER TABLE content_reports
  ADD COLUMN snapshot_org_name         text CONSTRAINT content_reports_snapshot_org_name_chk CHECK (char_length(snapshot_org_name) <= 200),
  ADD COLUMN snapshot_org_description  text CONSTRAINT content_reports_snapshot_org_description_chk CHECK (char_length(snapshot_org_description) <= 5000),
  ADD COLUMN snapshot_org_website_url  text CONSTRAINT content_reports_snapshot_org_website_url_chk CHECK (char_length(snapshot_org_website_url) <= 2048),
  ADD COLUMN snapshot_org_social_links jsonb CONSTRAINT content_reports_snapshot_org_social_links_chk CHECK (jsonb_typeof(snapshot_org_social_links) = 'object'),
  ADD COLUMN snapshot_org_logo_file_id uuid,
  DROP CONSTRAINT content_reports_snapshot_target_chk,
  DROP CONSTRAINT content_reports_snapshot_at_chk,
  ADD CONSTRAINT content_reports_snapshot_target_chk CHECK (snapshot_at IS NULL OR target_type IN ('post', 'org')),
  ADD CONSTRAINT content_reports_snapshot_at_chk CHECK (snapshot_at IS NOT NULL
    OR (snapshot_title IS NULL AND snapshot_body_text IS NULL AND snapshot_opportunity IS NULL AND snapshot_event_id IS NULL
      AND snapshot_org_name IS NULL AND snapshot_org_description IS NULL AND snapshot_org_website_url IS NULL
      AND snapshot_org_social_links IS NULL AND snapshot_org_logo_file_id IS NULL)),
  ADD CONSTRAINT content_reports_snapshot_post_columns_chk CHECK (target_type = 'post'
    OR (snapshot_title IS NULL AND snapshot_body_text IS NULL AND snapshot_opportunity IS NULL AND snapshot_event_id IS NULL)),
  ADD CONSTRAINT content_reports_snapshot_org_columns_chk CHECK (target_type = 'org'
    OR (snapshot_org_name IS NULL AND snapshot_org_description IS NULL AND snapshot_org_website_url IS NULL
      AND snapshot_org_social_links IS NULL AND snapshot_org_logo_file_id IS NULL)),
  ADD CONSTRAINT content_reports_snapshot_org_name_required_chk CHECK (target_type <> 'org' OR snapshot_at IS NULL OR snapshot_org_name IS NOT NULL);

CREATE OR REPLACE FUNCTION public.content_reports_snapshot_guard() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.snapshot_title IS DISTINCT FROM OLD.snapshot_title
     OR NEW.snapshot_body_text IS DISTINCT FROM OLD.snapshot_body_text
     OR NEW.snapshot_opportunity IS DISTINCT FROM OLD.snapshot_opportunity
     OR NEW.snapshot_event_id IS DISTINCT FROM OLD.snapshot_event_id
     OR NEW.snapshot_org_name IS DISTINCT FROM OLD.snapshot_org_name
     OR NEW.snapshot_org_description IS DISTINCT FROM OLD.snapshot_org_description
     OR NEW.snapshot_org_website_url IS DISTINCT FROM OLD.snapshot_org_website_url
     OR NEW.snapshot_org_social_links IS DISTINCT FROM OLD.snapshot_org_social_links
     OR NEW.snapshot_org_logo_file_id IS DISTINCT FROM OLD.snapshot_org_logo_file_id
     OR NEW.snapshot_at IS DISTINCT FROM OLD.snapshot_at THEN
    RAISE EXCEPTION 'UPDATE of content_reports.% is not allowed: the snapshot of the reported target is written with the report only', OLD.id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'content_reports_snapshot_frozen';
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER content_reports_snapshot_guard ON content_reports;
CREATE TRIGGER content_reports_snapshot_guard
  BEFORE UPDATE OF snapshot_title, snapshot_body_text, snapshot_opportunity, snapshot_event_id,
    snapshot_org_name, snapshot_org_description, snapshot_org_website_url, snapshot_org_social_links, snapshot_org_logo_file_id,
    snapshot_at ON content_reports
  FOR EACH ROW EXECUTE FUNCTION public.content_reports_snapshot_guard();

CREATE TABLE org_dropped_logos (
  file_id    uuid PRIMARY KEY CONSTRAINT org_dropped_logos_file_id_fkey REFERENCES files (id) ON DELETE CASCADE,
  org_id     uuid NOT NULL CONSTRAINT org_dropped_logos_org_id_fkey REFERENCES organizations (id),
  dropped_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX org_dropped_logos_org_idx ON org_dropped_logos (org_id);
CREATE INDEX org_dropped_logos_dropped_idx ON org_dropped_logos (dropped_at);
-- SIUD like every hard-delete table (02 §0.5); UPDATE is the retention
-- sweep's row lock (SELECT … FOR UPDATE SKIP LOCKED), never an UPDATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON org_dropped_logos TO app_rw;

ALTER TABLE files ADD COLUMN retired_at timestamptz;
ALTER TABLE files ADD CONSTRAINT files_retired_public_chk CHECK (retired_at IS NULL OR public_key IS NULL);

-- The kept logos' retention, and the text of the reports' policy names the
-- copy (only the seeded, unapproved row: 000161 pattern).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('org_logo_dropped', 'feed', 'org_dropped_logos', '12 months', 'closed_at', 'hard_delete', 'Moderasyon itirazı', true,
   'Açık şikâyeti olan kuruluşun değiştirdiği logolar 12 ay sonra silinir; logodan önce açılmış bir şikâyet açıkken ya da çözümünden sonraki 12 ay içinde beklenir.')
ON CONFLICT (code) DO NOTHING;
UPDATE public.retention_policies
   SET description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle ya da kuruluş profiliyle ve mesaj şikâyetinde saklanan mesaj kopyasıyla (şikâyet edilen mesaj ve öncesindeki en çok 5 mesaj) birlikte silinir.'
 WHERE code = 'content_report' AND description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle ve mesaj şikâyetinde saklanan mesaj kopyasıyla (şikâyet edilen mesaj ve öncesindeki en çok 5 mesaj) birlikte silinir.'
   AND NOT legal_approved;
COMMIT;
