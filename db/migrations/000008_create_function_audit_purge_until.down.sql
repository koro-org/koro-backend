-- 000008_create_function_audit_purge_until.down.sql
BEGIN;
DROP FUNCTION IF EXISTS public.audit_purge_until(date);
COMMIT;
