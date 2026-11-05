-- 000179_seed_web_push_subscription_policy.down.sql
-- Removes the policy unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then; decide by hand).
-- Subscriptions are then kept while the account lives, as before BE-177.
BEGIN;
SET LOCAL lock_timeout = '5s';
DELETE FROM public.retention_policies WHERE code = 'web_push_subscription'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'web_push_subscription');
COMMIT;
