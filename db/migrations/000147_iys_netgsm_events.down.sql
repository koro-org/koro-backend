-- 000147_iys_netgsm_events.down.sql
-- DATA LOSS: the stored Netgsm webhook items (at most seven days). The read
-- position of the Netgsm feeds goes too (a sequence that starts again must
-- not be read from an old cursor): the next consent.iys_status_sync reads
-- the feed from its start and reconciles every grant in force (a first
-- read); an upload whose answer is lost is sent again; the retention
-- policy of the table is removed. The ledger is untouched.
BEGIN;
SET LOCAL lock_timeout = '5s';
DELETE FROM iys_change_feeds WHERE feed LIKE 'netgsm:%';
DROP TABLE iys_netgsm_events;
-- The policy goes unless a destruction record refers to it (destruction_logs
-- is append-only and keeps its policy: the row stays then).
DELETE FROM public.retention_policies WHERE code = 'iys_netgsm_event'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'iys_netgsm_event');
COMMIT;
