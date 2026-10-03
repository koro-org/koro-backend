-- 000074_create_moderation_actions.down.sql
-- DATA LOSS: the whole moderation log.
BEGIN;
DROP TABLE IF EXISTS moderation_actions;
DROP FUNCTION IF EXISTS public.moderation_actions_forbid_mutation();
COMMIT;
