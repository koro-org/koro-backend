-- 000081_seed_talent_signal_defs.up.sql (02 §15.2, R-003 §3.3). The starting
-- signal catalogue. The weights are a product decision to be tuned with
-- pilot data; nothing is written to the ledger while
-- talent.default_enabled = false (T-32). Caps are magnitudes in cp (1/100
-- point): "2 check-ins a day" = 2 × 500 = 1000; the -400/week of
-- event_no_show is weekly_cap_cp 400. Caps that are not per day/week ("once
-- per section", "once a year", "at most 2 clubs", "once per post") are
-- service rules (unique source, per_source cap), so those columns are NULL.
-- opportunity_application is phase 3 (enabled = false).
-- ON CONFLICT DO NOTHING: re-running never overwrites a tuned row.
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO talent.signal_defs (code, category, description, base_points_cp, daily_cap_cp, weekly_cap_cp, half_life_days, excluded_when_sensitive, enabled) VALUES
  ('event_checkin',                'participation', 'Etkinlikte görevli okutmasıyla check-in (organizatör/görevli kendi etkinliğinde 0).', 500, 1000, NULL, 120, true, true),
  ('event_registration_confirmed', 'participation', 'Etkinlik kaydının onaylanması; kayıt iptalinde ters kayıt.',                         100,  300,  500,  45, true, true),
  ('event_no_show',                'participation', 'Katılımı kapatılmış etkinlikte gelmeme; kategori net değeri 0''ın altına inmez.',   -200, NULL, 400,  60, true, true),
  ('opportunity_application',      'intent',        'Fırsat ilanına başvuru (Faz 3).',                                                  300,  900, NULL,  90, true, false),
  ('profile_section_complete',     'profile',       'Profil bölümünün tamamlanması (bölüm başına bir kez).',                            200, NULL, NULL, NULL, true, true),
  ('profile_refresh',              'profile',       '"Hâlâ öğrenci misin?" yenilemesi (yılda bir).',                                    200, NULL, NULL, 365, true, true),
  ('club_membership_active',       'community',     'Etkin kulüp üyeliği (en çok 2 kulüp; hassas kulüp sinyal üretmez).',               200, NULL, NULL, NULL, true, true),
  ('feed_like',                    'engagement',    'Gönderi beğenisi (gönderi başına bir kez).',                                        10,   50, NULL,  30, true, true),
  ('feed_save',                    'engagement',    'Gönderi kaydetme (gönderi başına bir kez).',                                        10,   50, NULL,  30, true, true),
  ('login_active_day',             'engagement',    'Aktif gün (günde bir).',                                                            20,   20,  100,  30, true, true)
ON CONFLICT (code) DO NOTHING;
COMMIT;
