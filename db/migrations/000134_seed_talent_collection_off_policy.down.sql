-- 000134_seed_talent_collection_off_policy.down.sql
-- Removes the policy unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then; decide by hand).
BEGIN;
SET LOCAL lock_timeout = '5s';
DELETE FROM public.retention_policies WHERE code = 'talent_collection_off'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'talent_collection_off');
COMMIT;
