-- 000160_index_posts_event_shares.up.sql (BE-150). When a shared event
-- leaves the public web, is cancelled or changes what its post card shows,
-- the web revalidation reads the published posts sharing it (feed
-- ListEventShares: event_id, newest first); without this index every event
-- edit scanned the published posts. Partial: only published event shares.
-- Live table: one statement, CONCURRENTLY, no BEGIN (05 §7.2).
CREATE INDEX CONCURRENTLY IF NOT EXISTS posts_event_idx
  ON posts (event_id, published_at DESC, id DESC)
  WHERE status = 'published' AND event_id IS NOT NULL;
