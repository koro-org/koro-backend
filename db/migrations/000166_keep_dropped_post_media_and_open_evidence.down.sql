-- 000166_keep_dropped_post_media_and_open_evidence.down.sql
-- DATA LOSS: the dropped images' rows (their files stay attached to their
-- posts; the code before BE-158 neither shows nor purges them, the purge of
-- a removed post leaves them behind) and the replacement times of the
-- delivery evidence. A rejected evidence is replaceable on any line again
-- (000163). Roll back only together with the code.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER order_lines_evidence_open_guard ON order_lines;
DROP FUNCTION public.order_lines_evidence_open_guard();
ALTER TABLE order_lines DROP CONSTRAINT order_lines_evidence_replaced_chk;
ALTER TABLE order_lines DROP COLUMN fulfillment_evidence_replaced_at;
DROP TABLE post_dropped_media;
-- The policy goes unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then).
DELETE FROM public.retention_policies WHERE code = 'post_media_dropped'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'post_media_dropped');
COMMIT;
