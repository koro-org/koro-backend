-- 000067_create_post_media.down.sql
-- DATA LOSS: the media links of every post (the files rows stay).
BEGIN;
DROP TABLE IF EXISTS post_media;
COMMIT;
