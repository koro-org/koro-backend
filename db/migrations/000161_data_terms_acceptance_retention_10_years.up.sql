-- 000161_data_terms_acceptance_retention_10_years.up.sql (BE-152; user and lawyer
-- decision Q-18, 2026-10-31; 00 T-374). The terms of use acceptances are the
-- proof of the service contract: kept while the account lives, then 10 years
-- after its erasure (TBK m.146, the general limitation period of contract
-- claims) instead of the 3 years seeded by 000148. Only the seeded row
-- changes: a row an operator has already edited or legally approved is left
-- alone (000121 pattern); the approval itself (approved_ref) stays an act of
-- the KVKK admin screen (T-35).
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE public.retention_policies SET retention = interval '10 years'
 WHERE code = 'terms_acceptance' AND retention = interval '3 years' AND NOT legal_approved;
UPDATE public.retention_policies
   SET legal_basis = 'Sözleşme ispatı (TBK m.146, 10 yıllık genel zamanaşımı); hukukçu görüşü 2026-10-31 (Q-18)',
       description_tr = 'Kullanım koşulları kabul kayıtları hesap yaşadıkça saklanır, hesap silindikten 10 yıl sonra silinir.'
 WHERE code = 'terms_acceptance' AND retention = interval '10 years' AND NOT legal_approved;
COMMIT;
