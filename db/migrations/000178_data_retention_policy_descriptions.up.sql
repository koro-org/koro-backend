-- 000178_data_retention_policy_descriptions.up.sql (BE-176; DOC-003 finding 3).
-- The KVKK admin screen shows the seeded policy text to the DPO for the
-- legal approval (T-35); two texts said less than the code does. Only the
-- seeded, unapproved rows change (000161/000168 pattern: matched on the old
-- text and NOT legal_approved); no period, anchor or method changes, so no
-- deletion changes.
--   * content_report: since 000174 a message report keeps a copy of the
--     reported message and at most the 5 before it (message_report_messages,
--     ON DELETE CASCADE from content_reports); it goes with its report under
--     this policy. The text named the post copy of 000168 only.
--   * dsr_export_file: kvkk.purge_exports counts the period from the
--     request's completion (data_subject_requests.completed_at), not from
--     the file's created_at, and never longer than KVKK_EXPORT_RETENTION
--     (≤ 7 days, T-83/T-192). The anchor stays 'created_at': no allowed
--     anchor names the completion of the request (a new one would be a
--     schema change), the job does not read the anchor, and the file is
--     made after the completion, so the completion is the earlier, shorter
--     reading. The text says where the period really starts.
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE public.retention_policies
   SET description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle ve mesaj şikâyetinde saklanan mesaj kopyasıyla (şikâyet edilen mesaj ve öncesindeki en çok 5 mesaj) birlikte silinir.'
 WHERE code = 'content_report' AND description_tr = 'Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle birlikte silinir.'
   AND NOT legal_approved;
UPDATE public.retention_policies
   SET description_tr = 'İlgili kişi başvurusu dışa aktarma dosyası silinir. Süre dosyanın oluşturulmasından değil, başvurunun tamamlanmasından sayılır ve en çok 7 gündür.'
 WHERE code = 'dsr_export_file' AND description_tr = 'İlgili kişi başvurusu dışa aktarma dosyası silinir.'
   AND NOT legal_approved;
COMMIT;
