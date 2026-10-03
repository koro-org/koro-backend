-- 000069_create_post_saves.up.sql (02 §12.5). A user's saved post. Personal
-- data: deleted with the account or the post (ON DELETE CASCADE).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE post_saves (
  user_id     uuid NOT NULL CONSTRAINT post_saves_user_id_fkey REFERENCES users (id),
  post_id     uuid NOT NULL CONSTRAINT post_saves_post_id_fkey REFERENCES posts (id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT post_saves_pkey PRIMARY KEY (user_id, post_id)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON post_saves TO app_rw;
COMMIT;
