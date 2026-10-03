-- 000090_seed_retention_policies.up.sql (02 §24.2, §16.2). The proposed
-- retention periods: every row starts with legal_approved = false (T-35);
-- the lawyer / the accountant approves or changes them in the table, the
-- code does not change. ON CONFLICT DO NOTHING: re-running never overwrites
-- an approved or edited row.
-- Where 02 §24.2 leaves a value open:
--   * audit_events: method hard_delete, done by audit_purge_until (000008);
--   * financial_records: the anchor is created_at, rounded up to the end of
--     the calendar year by the sweeper; owner_module ledger (the tables span
--     orders, payments, ledger, billing and parties);
--   * entitlement_other: no legal basis in 02 → "proof of use" (proposal);
--   * dsr_erasure: a category of destruction_logs only, no sweeper; target users;
--   * dormant_account: no period decided; disabled, with the placeholder
--     period of 100 years so that enabling it by mistake destroys nothing.
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('outbox_published',             'platform',     'outbox',                                    '7 days',    'published_at',        'hard_delete',  'Operasyonel',                          true, 'Yayımlanmış outbox satırları silinir.'),
  ('webhook_payload',              'platform',     'webhook_inbox',                             '90 days',   'created_at',          'null_columns', 'R-003 §5.4',                           true, 'Gelen webhook gövdesi ve başlıkları boşaltılır.'),
  ('webhook_row',                  'platform',     'webhook_inbox',                             '2 years',   'created_at',          'hard_delete',  'Hata ayıklama/ispat',                  true, 'Gelen webhook satırları silinir.'),
  ('idempotency',                  'platform',     'idempotency_keys',                          '24 hours',  'expires_at',          'hard_delete',  'Operasyonel',                          true, 'Süresi dolan idempotency kayıtları silinir.'),
  ('audit_events',                 'audit',        'audit_events',                              '2 years',   'created_at',          'hard_delete',  'Teknik rapor §8.3; hukukçu',           true, 'Denetim kayıtları yalnız audit_purge_until ile, imzalı günlük köke kadar silinir.'),
  ('audit_roots',                  'audit',        'audit_daily_roots',                         '10 years',  'created_at',          'hard_delete',  'Bütünlük ispatı',                      true, 'İmzalı günlük denetim kökleri silinir.'),
  ('files_orphan',                 'files',        'files:*',                                   '24 hours',  'created_at',          'file_delete',  'Bağlanmamış yükleme',                  true, 'Hiçbir kayda bağlanmamış yüklemeler silinir.'),
  ('edu_document',                 'identity',     'files:edu_document',                        '30 days',   'decided_at',          'file_delete',  'Veri minimizasyonu',                   true, 'Öğrenci belgesi dosyası karardan sonra silinir.'),
  ('event_export_file',            'events',       'files:event_export',                        '7 days',    'expires_at',          'file_delete',  'R-003 §2.6',                           true, 'Etkinlik dışa aktarma dosyası silinir.'),
  ('dsr_export_file',              'kvkk',         'files:dsr_export',                          '30 days',   'created_at',          'file_delete',  'Teslim süresi',                        true, 'İlgili kişi başvurusu dışa aktarma dosyası silinir.'),
  ('consent_proof',                'consent',      'consents, iys_submissions',                 '3 years',   'withdrawn_or_erased', 'hard_delete',  '6563 / Ticari İletişim Yön. (ispat)',  true, 'Rıza ve İYS kayıtları geri çekme veya hesap silmeden sonra silinir.'),
  ('session_expired',              'identity',     'sessions',                                  '30 days',   'expires_at',          'hard_delete',  'Güvenlik incelemesi',                  true, 'Süresi dolmuş veya iptal edilmiş oturumlar silinir.'),
  ('email_token',                  'identity',     'email_tokens',                              '30 days',   'expires_at',          'hard_delete',  'Operasyonel',                          true, 'Kullanılmış veya süresi dolmuş e-posta tokenları silinir.'),
  ('edu_verification_history',     'identity',     'edu_verifications',                         '1 year',    'ended_at',            'hard_delete',  'Yenileme geçmişi',                     true, 'Biten öğrenci doğrulama kayıtları silinir.'),
  ('role_history',                 'identity',     'platform_role_assignments',                 '3 years',   'ended_at',            'hard_delete',  'Yetki gözden geçirme ispatı',          true, 'Biten platform rolü atamaları silinir.'),
  ('notification_delivery',        'notify',       'notification_deliveries',                   '12 months', 'created_at',          'hard_delete',  'R-003 §5.4',                           true, 'Bildirim teslim kayıtları silinir.'),
  ('email_suppression',            'notify',       'email_suppressions',                        '3 years',   'created_at',          'hard_delete',  'Teslim itibarı',                       true, 'E-posta engelleme kayıtları silinir.'),
  ('push_token',                   'notify',       'push_tokens',                               '90 days',   'last_seen_at',        'hard_delete',  'Kullanılmayan cihaz',                  true, 'Kullanılmayan push tokenları silinir.'),
  ('in_app_notification',          'notify',       'in_app_notifications',                      '6 months',  'created_at',          'hard_delete',  'UX',                                   true, 'Uygulama içi bildirimler silinir.'),
  ('membership_ended',             'orgs',         'org_members',                               '6 months',  'ended_at',            'hard_delete',  'Uyuşmazlık penceresi',                 true, 'Biten üyelikler silinir.'),
  ('membership_ended_sensitive',   'orgs',         'org_members',                               '0',         'ended_at',            'hard_delete',  'Özel nitelikli veri minimizasyonu',    true, 'Hassas kulüpte biten üyelik bitişte hemen silinir.'),
  ('org_invitation',               'orgs',         'org_invitations',                           '30 days',   'expires_at',          'hard_delete',  'Operasyonel',                          true, 'Süresi dolan davetler silinir.'),
  ('club_claim',                   'directory',    'club_claims',                               '1 year',    'decided_at',          'hard_delete',  'Sahiplenme itirazı',                   true, 'Karara bağlanmış kulüp sahiplenme başvuruları (kanıt dosyasıyla) silinir.'),
  ('access_grant_revoked',         'students',     'profile_access_grants',                     '6 months',  'ended_at',            'hard_delete',  'İspat',                                true, 'Geri alınmış profil erişim izinleri silinir.'),
  ('event_form_answers',           'events',       'event_registrations.answers',               '90 days',   'event_end',           'null_columns', 'R-003 §5.4',                           true, 'Etkinlik formu cevapları boşaltılır.'),
  ('event_form_answers_sensitive', 'events',       'event_registrations.answers_sensitive_enc', '30 days',   'event_end',           'null_columns', 'R-003 §5.4',                           true, 'Özel nitelikli form cevapları boşaltılır.'),
  ('event_staff',                  'events',       'event_staff',                               '90 days',   'event_end',           'hard_delete',  'Etkinlik sonrası ihtiyaç yok',         true, 'Etkinlik görevlisi atamaları silinir.'),
  ('event_export_log',             'events',       'event_data_exports',                        '2 years',   'created_at',          'hard_delete',  'Sorumluluk ispatı',                    true, 'Etkinlik dışa aktarma kayıtları silinir.'),
  ('post_removed',                 'feed',         'posts',                                     '12 months', 'closed_at',           'hard_delete',  'Moderasyon itirazı',                   true, 'Kaldırılmış gönderiler medya ve etkileşimleriyle silinir.'),
  ('content_report',               'feed',         'content_reports',                           '12 months', 'closed_at',           'hard_delete',  'Moderasyon',                           true, 'Çözülmüş içerik şikâyetleri silinir.'),
  ('moderation_action',            'feed',         'moderation_actions',                        '2 years',   'created_at',          'hard_delete',  'Moderasyon ispatı',                    true, 'Moderasyon kararları silinir.'),
  ('outreach_dispatch',            'outreach',     'outreach_dispatches',                       '2 years',   'created_at',          'hard_delete',  'Hak kullanımı ispatı',                 true, 'Hedefli bildirim gönderim kayıtları silinir.'),
  ('contact_request_closed',       'messaging',    'contact_requests',                          '12 months', 'closed_at',           'hard_delete',  'Uyuşmazlık',                           true, 'Kapanmış iletişim talepleri silinir.'),
  ('talent_ledger',                'talent',       'talent.point_events',                       '24 months', 'created_at',          'hard_delete',  'R-003 §5.4',                           true, 'Talent puan defteri satırları silinir.'),
  ('talent_counters',              'talent',       'talent.daily_counters',                     '14 days',   'created_at',          'hard_delete',  'Tavan hesabı',                         true, 'Talent günlük sayaçları silinir.'),
  ('talent_runs',                  'talent',       'talent.score_runs',                         '1 year',    'created_at',          'hard_delete',  'Operasyonel',                          true, 'Talent skor çalıştırma kayıtları silinir.'),
  ('talent_neutralization',        'talent',       'talent.neutralizations',                    '3 years',   'ended_at',            'hard_delete',  'İtiraza uyum ispatı',                  true, 'Kaldırılmış talent nötrleme kayıtları silinir.'),
  ('entitlement_other',            'entitlements', 'entitlements',                              '2 years',   'expires_at',          'hard_delete',  'Hak kullanımı ispatı (öneri)',         true, 'Sipariş dışı haklar (yönetici/promosyon) silinir.'),
  ('financial_records',            'ledger',       'orders, order_lines, payments, payment_splits, refunds, incoming_bank_transfers, ledger_transactions, ledger_entries, payouts, payout_items, invoices, invoice_lines, legal_parties, legal_party_documents, payees, payee_bank_accounts, files:financial',
                                                                                                 '10 years',  'created_at',          'hard_delete',  'TTK md. 82, VUK; mali müşavir',        true, 'Mali kayıtlar ve mali dosyalar; süre takvim yılı sonuna yuvarlanır.'),
  ('dsr_record',                   'kvkk',         'data_subject_requests',                     '3 years',   'closed_at',           'hard_delete',  'Hukukçu',                              true, 'Kapanmış ilgili kişi başvuruları silinir.'),
  ('destruction_log',              'kvkk',         'destruction_logs',                          '3 years',   'created_at',          'hard_delete',  'Teknik rapor §6.2',                    true, 'İmha kayıtları silinir.'),
  ('breach_record',                'kvkk',         'breach_incidents',                          '5 years',   'closed_at',           'hard_delete',  'Hukukçu',                              true, 'Kapanmış veri ihlali kayıtları silinir.'),
  ('dsr_erasure',                  'kvkk',         'users',                                     '0',         'created_at',          'hard_delete',  'Yalnız destruction_logs.policy_code için; sweeper yok', true, 'Hesap silme imha kaydı kategorisi; süpürücü yok.'),
  ('dormant_account',              'identity',     'users',                                     '100 years', 'last_seen_at',        'anonymize',    'Hukukçu kararına kadar kapalı',        false, 'Uzun süre pasif hesapların anonimleştirilmesi; süre belirlenmedi (yer tutucu), kapalı.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
