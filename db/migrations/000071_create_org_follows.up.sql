-- 000071_create_org_follows.up.sql (02 §12.7). A user follows an org
-- (manually or automatically through a membership / the university). A
-- sensitive club membership never creates an automatic follow (the list
-- must not leak the membership; service rule). The org sees the count only
-- (k >= 10), never the followers.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE org_follows (
  user_id     uuid NOT NULL CONSTRAINT org_follows_user_id_fkey REFERENCES users (id),
  org_id      uuid NOT NULL CONSTRAINT org_follows_org_id_fkey REFERENCES organizations (id),
  source      text NOT NULL CONSTRAINT org_follows_source_chk CHECK (source IN ('manual','auto_membership','auto_university')),
  notify      boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_follows_pkey PRIMARY KEY (user_id, org_id)
);
CREATE INDEX org_follows_org_idx ON org_follows (org_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON org_follows TO app_rw;
COMMIT;
