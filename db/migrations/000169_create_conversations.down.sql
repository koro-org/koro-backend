-- 000169_create_conversations.down.sql
-- DATA LOSS: every conversation, its messages and its participants' read and
-- mute states. The contact requests stay (an accepted request keeps its level
-- 3 grant). Roll back only together with the code that writes them.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE conversation_user_states;
DROP TABLE conversation_messages;
DROP TABLE conversations;
-- The policy goes unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then).
DELETE FROM public.retention_policies WHERE code = 'conversation_closed'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'conversation_closed');
COMMIT;
