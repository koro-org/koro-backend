-- 000012_create_notice_versions.down.sql
-- DATA LOSS: every notice and consent text version (the proof of what was shown).
BEGIN;
DROP TABLE IF EXISTS notice_versions;
DROP FUNCTION IF EXISTS public.notice_versions_guard();
COMMIT;
