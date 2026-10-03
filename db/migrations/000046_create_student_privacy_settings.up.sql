-- 000046_create_student_privacy_settings.up.sql (02 §9.2). Privacy choices of
-- a student; every flag defaults to the private side. discoverable and
-- show_contact_on_grant mirror consents and change only in the same
-- transaction as the consent row (application; the DB cannot see it).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE student_privacy_settings (
  user_id                    uuid PRIMARY KEY CONSTRAINT student_privacy_settings_user_id_fkey REFERENCES users (id),
  discoverable               boolean NOT NULL DEFAULT false,   -- level 2; source: consents('discovery_profile')
  show_contact_on_grant      boolean NOT NULL DEFAULT false,   -- contact details at level 3 (consents 'contact_info_share')
  show_clubs_on_grant        boolean NOT NULL DEFAULT false,   -- non-sensitive club memberships at level 3
  activity_ranking_choice    text NOT NULL DEFAULT 'default'
                               CONSTRAINT student_privacy_settings_activity_ranking_choice_chk CHECK (activity_ranking_choice IN ('default','enabled','disabled')),
  created_at                 timestamptz NOT NULL DEFAULT now(),
  updated_at                 timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER student_privacy_settings_touch_updated_at BEFORE UPDATE ON student_privacy_settings
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON student_privacy_settings TO app_rw;
COMMIT;
