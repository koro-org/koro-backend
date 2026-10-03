-- 000066_create_posts.down.sql
-- DATA LOSS: every post of every org.
BEGIN;
DROP TABLE IF EXISTS posts;
COMMIT;
