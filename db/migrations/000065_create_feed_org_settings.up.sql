-- 000065_create_feed_org_settings.up.sql (02 §12.1). The feed trust level of
-- an org: a 'new' org's posts go to pending_review, after the configured
-- number of approved posts it becomes 'trusted'; a restriction or suspension
-- carries its reason. No row = 'new'. Lives as long as the org (no DELETE).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE feed_org_settings (
  org_id                uuid PRIMARY KEY CONSTRAINT feed_org_settings_org_id_fkey REFERENCES organizations (id),
  trust_level           text NOT NULL DEFAULT 'new'
                          CONSTRAINT feed_org_settings_trust_level_chk CHECK (trust_level IN ('new','trusted','restricted','suspended')),
  approved_posts_count  integer NOT NULL DEFAULT 0 CONSTRAINT feed_org_settings_approved_posts_count_chk CHECK (approved_posts_count >= 0),
  restricted_until      timestamptz,
  reason                text CONSTRAINT feed_org_settings_reason_chk CHECK (char_length(reason) <= 500),
  updated_by            uuid CONSTRAINT feed_org_settings_updated_by_fkey REFERENCES users (id),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT feed_org_settings_restriction_reason_chk CHECK (trust_level NOT IN ('restricted','suspended') OR reason IS NOT NULL)
);
CREATE TRIGGER feed_org_settings_touch_updated_at BEFORE UPDATE ON feed_org_settings
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON feed_org_settings TO app_rw;
COMMIT;
