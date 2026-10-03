-- 000067_create_post_media.up.sql (02 §12.3). Images of a post (at most 10,
-- positions 0–9); a file belongs to one post. Deleted with the post (ON
-- DELETE CASCADE; the stored object goes through files.Delete). Publishing
-- needs files.scan_status = 'clean' for every medium (service rule).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE post_media (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  post_id     uuid NOT NULL CONSTRAINT post_media_post_id_fkey REFERENCES posts (id) ON DELETE CASCADE,
  file_id     uuid NOT NULL CONSTRAINT post_media_file_id_uq UNIQUE CONSTRAINT post_media_file_id_fkey REFERENCES files (id),
  position    smallint NOT NULL CONSTRAINT post_media_position_chk CHECK (position BETWEEN 0 AND 9),
  alt_text    text CONSTRAINT post_media_alt_text_chk CHECK (char_length(alt_text) <= 300),
  width       integer CONSTRAINT post_media_width_chk CHECK (width > 0),
  height      integer CONSTRAINT post_media_height_chk CHECK (height > 0),
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT post_media_post_position_uq UNIQUE (post_id, position)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON post_media TO app_rw;
COMMIT;
