-- 000162_create_unsubscribe_links.down.sql
-- DATA LOSS: every opaque unsubscribe link. The links of the commercial
-- e-mails sent since BE-153 stop working (422 consent.invalid_unsubscribe_link):
-- their recipients withdraw in the app's settings, a mailbox provider's
-- one-click POST fails. Roll back only together with the code that mints
-- them. The consent ledger is untouched.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE unsubscribe_links;
-- The policy goes unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then).
DELETE FROM public.retention_policies WHERE code = 'unsubscribe_link'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'unsubscribe_link');
COMMIT;
