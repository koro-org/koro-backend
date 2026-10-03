-- 000069_create_post_saves.down.sql
-- DATA LOSS: every saved post.
BEGIN;
DROP TABLE IF EXISTS post_saves;
COMMIT;
