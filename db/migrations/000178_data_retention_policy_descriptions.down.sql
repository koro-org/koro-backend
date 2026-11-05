-- 000178_data_retention_policy_descriptions.down.sql
-- Back to the texts of 000168 and 000090, word for word (000168's down
-- matches its own text), unless the row was legally approved or edited
-- since (then decide by hand).
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE public.retention_policies
   SET description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle birlikte silinir.'
 WHERE code = 'content_report' AND description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle ve mesaj şikâyetinde saklanan mesaj kopyasıyla (şikâyet edilen mesaj ve öncesindeki en çok 5 mesaj) birlikte silinir.'
   AND NOT legal_approved;
UPDATE public.retention_policies
   SET description_tr = 'İlgili kişi başvurusu dışa aktarma dosyası silinir.'
 WHERE code = 'dsr_export_file' AND description_tr = 'İlgili kişi başvurusu dışa aktarma dosyası silinir. Süre dosyanın oluşturulmasından değil, başvurunun tamamlanmasından sayılır ve en çok 7 gündür.'
   AND NOT legal_approved;
COMMIT;
