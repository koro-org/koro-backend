-- 000081_seed_talent_signal_defs.down.sql
-- DATA LOSS: the seeded signal definitions (rows added later stay).
BEGIN;
DELETE FROM talent.signal_defs WHERE code IN (
  'event_checkin', 'event_registration_confirmed', 'event_no_show', 'opportunity_application', 'profile_section_complete',
  'profile_refresh', 'club_membership_active', 'feed_like', 'feed_save', 'login_active_day');
COMMIT;
