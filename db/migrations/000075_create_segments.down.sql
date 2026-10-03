-- 000075_create_segments.down.sql
-- DATA LOSS: every saved segment.
BEGIN;
DROP TABLE IF EXISTS segments;
COMMIT;
