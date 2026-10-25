-- 000146_iys_change_feeds.down.sql
-- DATA LOSS: the read positions of the İYS change feeds. The next run reads
-- the feed from the start of its window and reconciles every grant in force
-- (a first read), so no refusal is lost; the ledger is untouched.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE iys_change_feeds;
COMMIT;
