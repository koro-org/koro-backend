-- 000146_iys_change_feeds.up.sql (BE-105; 00 T-354; 02 §4.5 doc patch).
-- consent.iys_status_sync reads the refusals recipients give İYS itself from
-- the İYS adapter's change feed (internal/platform/iys: the official API's
-- change list, or what an integrator pushed): one row per feed (an İYS
-- brand of an adapter, e.g. 'iys:700123/700124'):
--   * cursor_token: the adapter's opaque position after the last applied
--     page (İYS's "after": creation time + transaction ID; no personal data).
--   * horizon: every change the feed recorded before it has been applied;
--     a late or concurrent run never moves it back (the store's upsert).
--   * sweep_due_at: the feed lost changes (first read, or a horizon older
--     than the feed's window: İYS keeps seven days) and a full
--     reconciliation of every grant in force is due since then; NULL when
--     none is. The channel states (iys_status_syncs) are not written while
--     it is due.
-- No personal data: codes, an opaque cursor and timestamps.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE iys_change_feeds (
  feed          text PRIMARY KEY CONSTRAINT iys_change_feeds_feed_chk CHECK (feed ~ '^[a-z0-9][a-z0-9:/._-]{0,99}$'),
  cursor_token  text NOT NULL DEFAULT ''
                CONSTRAINT iys_change_feeds_cursor_chk CHECK (char_length(cursor_token) <= 1000 AND cursor_token ~ '^[\x21-\x7e]*$'),
  horizon       timestamptz,
  sweep_due_at  timestamptz,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT iys_change_feeds_cursor_horizon_chk CHECK (cursor_token = '' OR horizon IS NOT NULL)
);
CREATE TRIGGER iys_change_feeds_touch_updated_at BEFORE UPDATE ON iys_change_feeds
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON iys_change_feeds TO app_rw;
COMMIT;
