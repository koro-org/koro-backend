-- 000068_create_post_reactions.up.sql (02 §12.4). A user's like of a post.
-- The author org sees the count only, never who liked. Personal data:
-- deleted with the account or the post (ON DELETE CASCADE).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE post_reactions (
  post_id     uuid NOT NULL CONSTRAINT post_reactions_post_id_fkey REFERENCES posts (id) ON DELETE CASCADE,
  user_id     uuid NOT NULL CONSTRAINT post_reactions_user_id_fkey REFERENCES users (id),
  kind        text NOT NULL DEFAULT 'like' CONSTRAINT post_reactions_kind_chk CHECK (kind IN ('like')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT post_reactions_pkey PRIMARY KEY (post_id, user_id)
);
CREATE INDEX post_reactions_user_idx ON post_reactions (user_id, created_at DESC);
GRANT SELECT, INSERT, UPDATE, DELETE ON post_reactions TO app_rw;
COMMIT;
