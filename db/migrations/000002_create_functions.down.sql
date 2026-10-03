-- 000002_create_functions.down.sql
BEGIN;
DROP FUNCTION IF EXISTS public.koro_iban_tr_valid(text);
DROP FUNCTION IF EXISTS public.koro_policy_close_only();
DROP FUNCTION IF EXISTS public.koro_forbid_mutation();
DROP FUNCTION IF EXISTS public.koro_touch_updated_at();
DROP FUNCTION IF EXISTS public.koro_norm(text);
COMMIT;
