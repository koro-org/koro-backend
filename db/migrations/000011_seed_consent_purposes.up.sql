-- 000011_seed_consent_purposes.up.sql (02 §4.1 seed). The purposes of v1.
-- ON CONFLICT DO NOTHING: re-running never overwrites a row an admin edited
-- (consent.purpose.manage). activity_ranking starts passive and its legal
-- basis is an open question (T-32): no consent is recorded for it, and the
-- talent ledger is not written, until the decision is made.
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO consent_purposes (code, description_tr, legal_basis, requires_iys, iys_channel, scope_type, active) VALUES
  ('commercial_email',   'Kampanya, duyuru ve fırsat içeren ticari elektronik iletileri e-posta ile alma izni (İYS kaydı yapılır).',      'explicit_consent', true,  'email', 'none',         true),
  ('commercial_sms',     'Kampanya, duyuru ve fırsat içeren ticari elektronik iletileri SMS ile alma izni (v1 sürümünde SMS kanalı yoktur; kayıt hazırdır).', 'explicit_consent', true,  'sms',   'none',         true),
  ('commercial_push',    'Kampanya, duyuru ve fırsat içeren ticari bildirimleri mobil bildirim olarak alma izni.',                         'explicit_consent', false, NULL,    'none',         true),
  ('discovery_profile',  'Profilimin takma adla, şirketlerin keşif ekranında görünmesine izin veriyorum (Seviye 2).',                      'explicit_consent', false, NULL,    'none',         true),
  ('contact_info_share', 'Bir iletişim talebini kabul ettiğimde iletişim bilgilerimin talep sahibiyle paylaşılmasına izin veriyorum.',    'explicit_consent', false, NULL,    'none',         true),
  ('event_sponsor_share','Katıldığım etkinliğin sponsoruyla katılım bilgilerimin paylaşılmasına izin veriyorum (kayıt başına).',           'explicit_consent', false, NULL,    'registration', true),
  ('activity_ranking',   'Etkinlik ve topluluk aktivitelerime dayalı sıralama (talent) için verilerimin işlenmesine izin veriyorum.',      'explicit_consent', false, NULL,    'none',         false)
ON CONFLICT (code) DO NOTHING;
COMMIT;
