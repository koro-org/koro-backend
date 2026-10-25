-- 000148_create_terms_acceptances.down.sql
-- DATA LOSS: every terms of use acceptance (the proof of which terms each
-- user accepted, and when). The retention policy goes unless a destruction
-- record refers to it (destruction_logs is append-only and keeps its policy:
-- the row stays then; decide by hand). The notices are untouched.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP FUNCTION public.terms_acceptances_purge(timestamptz, integer, boolean);
DROP TABLE terms_acceptances;
ALTER TABLE notice_versions DROP CONSTRAINT notice_versions_id_kind_sha_uq;
DELETE FROM public.retention_policies WHERE code = 'terms_acceptance'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'terms_acceptance');
COMMIT;
