-- 000060_seed_form_lint_rules.up.sql (02 §11.5, R-003 §2.6). The starting set
-- of KVKK lint rules of the event form. pattern is an RE2 expression (Go
-- regexp) applied to the koro_norm-ed label / help text, so it is written in
-- lower case ASCII without accents ("ç"→"c", "ş"→"s", "ı"→"i", ...).
-- Classes: 'blocked' (cannot be published, no override), 'warn_special'
-- (needs a reasoned override, answers are stored encrypted and short-lived),
-- 'review_identifier' (identifier that needs review). The rules are data: the
-- DPO deactivates a false positive or adds a rule (events.lint_rule.manage).
-- Fixed ids + ON CONFLICT DO NOTHING: re-running never overwrites a rule that
-- was edited or deactivated, and brings back a deleted seed row.
-- Class choices follow 02 §11.4 (ID / card / password = blocked; health,
-- diet, religion, disability = warn_special) and R-003 §2.6 for the other
-- special categories of KVKK md. 6 (blocked); the two documents disagree on
-- religion and on the national ID number (T-133).
-- BE-026 (in place, pre-pilot, R2): rule 1 also catches "Kimlik numaranız",
-- "TC No" and "nüfus cüzdanı" (the trailing \b missed every suffixed
-- "numarası"); rules 11-12 add pregnancy, psychiatric and "engel durumu".
-- internal/events.DefaultLintRules mirrors this file
-- (TestStore_GormSeedMatchesDefaultLintRules).
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO form_lint_rules (id, class, pattern, description_tr, active) VALUES
  ('01990000-0000-7000-8000-000000000001', 'blocked', '\btckn\b|\bt\.?\s?c\.?\s*(kimlik|vatandaslik|no\b|numara)|\bkimlik\s*(no\b|nu\b|numara)|\bvatandaslik\s*(no|numara)|\bnufus\s*cuzdan',
    'T.C. kimlik, vatandaşlık veya nüfus cüzdanı numarası istenemez (veri minimizasyonu, KVKK md. 4).', true),
  ('01990000-0000-7000-8000-000000000002', 'blocked', '\b(kredi|banka|debit)\s*kart|\bkart\s*(no|numara|bilgi)|\bcvv\b|\bcvc\b|\bson\s*kullanma\s*tarih',
    'Ödeme kartı bilgisi istenemez.', true),
  ('01990000-0000-7000-8000-000000000003', 'blocked', '\b(parola|sifre|password)|\bpin\s*kod',
    'Parola, şifre veya PIN istenemez.', true),
  ('01990000-0000-7000-8000-000000000004', 'blocked', '\biban\b|\bhesap\s*(no|numara)|\bbanka\s*hesab',
    'Banka hesabı veya IBAN istenemez.', true),
  ('01990000-0000-7000-8000-000000000005', 'blocked', '\bsabika|\bmahkumiyet|\bceza\s*(kayd|gecmis)|\badli\s*sicil',
    'Sabıka veya mahkûmiyet bilgisi (özel nitelikli veri, KVKK md. 6).', true),
  ('01990000-0000-7000-8000-000000000006', 'blocked', '\bbiyometrik|\bparmak\s*izi|\bgenetik|\bdna\b|\bretina|\byuz\s*tanima',
    'Biyometrik veya genetik veri (özel nitelikli veri, KVKK md. 6).', true),
  ('01990000-0000-7000-8000-000000000007', 'blocked', '\bsiyasi|\bsiyaset|\bparti\s*(uye|tercih)|\boy\s*(verdi|tercih)',
    'Siyasi düşünce veya parti tercihi (özel nitelikli veri, KVKK md. 6).', true),
  ('01990000-0000-7000-8000-000000000008', 'blocked', '\betnik|\birk(i|iniz|im)?\b',
    'Irk veya etnik köken (özel nitelikli veri, KVKK md. 6).', true),
  ('01990000-0000-7000-8000-000000000009', 'blocked', '\bcinsel\s*(yonelim|tercih|kimlik)|\blgbt',
    'Cinsel yönelim veya cinsel hayat (özel nitelikli veri, KVKK md. 6).', true),
  ('01990000-0000-7000-8000-000000000010', 'blocked', '\bsendika|\b(dernek|vakif)\s*uyel',
    'Sendika, dernek veya vakıf üyeliği (özel nitelikli veri, KVKK md. 6).', true),
  ('01990000-0000-7000-8000-000000000011', 'warn_special', '\bsaglik|\bhastalik|\bhasta\b|\balerji|\balerjen|\bilac|\bkronik|\brahatsizlik|\bameliyat|\btedavi|\bhamile|\bgebelik|\bpsikiyatr|\bdepresyon|\bpsikolojik\s*(destek|tedavi|sorun|durum|rahatsiz)',
    'Sağlık bilgisi (özel nitelikli veri): gerekçeli istisna, ayrı açık rıza ve kısa saklama gerekir.', true),
  ('01990000-0000-7000-8000-000000000012', 'warn_special', '\bengelli|\bengel\s*durum|\bengeliniz\b|\bozel\s*ihtiyac|\berisilebilirlik|\btekerlekli\s*sandalye',
    'Engellilik veya erişilebilirlik ihtiyacı (sağlık verisi): gerekçeli istisna gerekir.', true),
  ('01990000-0000-7000-8000-000000000013', 'warn_special', '\bdiyet|\bvegan|\bvejetaryen|\bhelal|\bglutensiz|\bgluten|\byemek\s*(tercih|ihtiyac|kisit)|\bozel\s*menu',
    'Yemek tercihi veya diyet; dolaylı olarak din ya da sağlık bilgisi açığa çıkarabilir.', true),
  ('01990000-0000-7000-8000-000000000014', 'warn_special', '\bkan\s*grub',
    'Kan grubu (sağlık verisi).', true),
  ('01990000-0000-7000-8000-000000000015', 'warn_special', '\bdin(i|sel|ini|iniz|im)?\b|\bmezhep|\binanc',
    'Din, mezhep veya inanç (özel nitelikli veri, KVKK md. 6): gerekçeli istisna gerekir.', true),
  ('01990000-0000-7000-8000-000000000016', 'review_identifier', '\bpasaport|\bpassport',
    'Pasaport numarası: yasal veya sigorta zorunluluğu yoksa istenmemeli.', true),
  ('01990000-0000-7000-8000-000000000017', 'review_identifier', '\behliyet\s*(no|numara)|\bsurucu\s*belge\s*(no|numara)',
    'Ehliyet numarası: zorunlu değilse istenmemeli.', true),
  ('01990000-0000-7000-8000-000000000018', 'review_identifier', '\bvergi\s*(no|numara|kimlik)',
    'Vergi numarası: zorunlu değilse istenmemeli.', true),
  ('01990000-0000-7000-8000-000000000019', 'review_identifier', '\bsgk\s*(no|sicil)|\bsosyal\s*guvenlik\s*(no|numara)',
    'SGK veya sosyal güvenlik numarası: zorunlu değilse istenmemeli.', true)
ON CONFLICT (id) DO NOTHING;
COMMIT;
