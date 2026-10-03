-- 000068_create_post_reactions.down.sql
-- DATA LOSS: every like.
BEGIN;
DROP TABLE IF EXISTS post_reactions;
COMMIT;
