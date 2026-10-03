-- 000070_create_post_hides.up.sql (02 §12.6). A post the user hid from their
-- feed. Personal data: deleted with the account or the post (ON DELETE
-- CASCADE).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE post_hides (
  user_id     uuid NOT NULL CONSTRAINT post_hides_user_id_fkey REFERENCES users (id),
  post_id     uuid NOT NULL CONSTRAINT post_hides_post_id_fkey REFERENCES posts (id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT post_hides_pkey PRIMARY KEY (user_id, post_id)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON post_hides TO app_rw;
COMMIT;
