-- 000161_data_terms_acceptance_retention_10_years.down.sql
-- Back to the 3 years of 000148, unless the row was legally approved since
-- (an approved period is the operator's; decide by hand).
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE public.retention_policies SET retention = interval '3 years',
       legal_basis = 'Sözleşme ispatı (TBK); hukukçu onayı bekler',
       description_tr = 'Kullanım koşulları kabul kayıtları hesap yaşadıkça saklanır, hesap silindikten 3 yıl sonra silinir.'
 WHERE code = 'terms_acceptance' AND retention = interval '10 years' AND NOT legal_approved;
COMMIT;
