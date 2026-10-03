-- 000011_seed_consent_purposes.down.sql
-- DATA LOSS: the seeded purpose rows. It fails while a consent still refers
-- to one of them (000013 is reverted first in a full down).
BEGIN;
DELETE FROM consent_purposes WHERE code IN (
  'commercial_email', 'commercial_sms', 'commercial_push', 'discovery_profile',
  'contact_info_share', 'event_sponsor_share', 'activity_ranking');
COMMIT;
