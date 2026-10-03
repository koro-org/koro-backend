-- 000070_create_post_hides.down.sql
-- DATA LOSS: every hidden-post choice.
BEGIN;
DROP TABLE IF EXISTS post_hides;
COMMIT;
