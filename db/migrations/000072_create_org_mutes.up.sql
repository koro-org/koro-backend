-- 000072_create_org_mutes.up.sql (02 §12.8). A user muted an org in the feed
-- (v1 has no user-to-user content, so no user blocks; contact_blocks covers
-- contact requests).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE org_mutes (
  user_id     uuid NOT NULL CONSTRAINT org_mutes_user_id_fkey REFERENCES users (id),
  org_id      uuid NOT NULL CONSTRAINT org_mutes_org_id_fkey REFERENCES organizations (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_mutes_pkey PRIMARY KEY (user_id, org_id)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON org_mutes TO app_rw;
COMMIT;
