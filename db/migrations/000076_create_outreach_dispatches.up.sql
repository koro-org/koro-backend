-- 000076_create_outreach_dispatches.up.sql (02 §13.2). The notification
-- dispatch of a targeted post (at most one per post). No row per recipient
-- (notification_deliveries holds those); the company sees the counts only
-- when k >= 10. Hard-deleted after 2 years (outreach_dispatch).
-- 06 H-38 (addition to 02): the post and the segment must belong to the
-- dispatching org; the composite FKs (post_id, org_id) → posts and
-- (segment_id, org_id) → segments replace the single-column FKs.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE outreach_dispatches (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id                uuid NOT NULL CONSTRAINT outreach_dispatches_org_id_fkey REFERENCES organizations (id),
  post_id               uuid NOT NULL CONSTRAINT outreach_dispatches_post_id_uq UNIQUE,
  segment_id            uuid,
  audience_keys         text[] NOT NULL,
  audience_dim_count    smallint NOT NULL CONSTRAINT outreach_dispatches_audience_dim_count_chk CHECK (audience_dim_count BETWEEN 1 AND 4),
  channels              text[] NOT NULL CONSTRAINT outreach_dispatches_channels_chk
                          CHECK (cardinality(channels) >= 1 AND channels <@ ARRAY['push','email','in_app']::text[]),
  title_snapshot        text NOT NULL CONSTRAINT outreach_dispatches_title_snapshot_chk CHECK (char_length(title_snapshot) <= 140),   -- notification title (not personal)
  deep_link             text NOT NULL CONSTRAINT outreach_dispatches_deep_link_chk CHECK (deep_link ~ '^/'),
  commercial            boolean NOT NULL,                     -- true ⇒ only to users with the commercial consent
  status                text NOT NULL DEFAULT 'scheduled'
                          CONSTRAINT outreach_dispatches_status_chk CHECK (status IN ('scheduled','running','completed','cancelled','failed')),
  eligible_count        integer CONSTRAINT outreach_dispatches_eligible_count_chk CHECK (eligible_count >= 0),
  consented_count       integer CONSTRAINT outreach_dispatches_consented_count_chk CHECK (consented_count >= 0),
  sent_count            integer CONSTRAINT outreach_dispatches_sent_count_chk CHECK (sent_count >= 0),
  entitlement_usage_id  uuid CONSTRAINT outreach_dispatches_entitlement_usage_id_fkey REFERENCES entitlement_usages (id),
  started_at            timestamptz,
  completed_at          timestamptz,
  last_error            text CONSTRAINT outreach_dispatches_last_error_chk CHECK (char_length(last_error) <= 2000),
  created_by            uuid NOT NULL CONSTRAINT outreach_dispatches_created_by_fkey REFERENCES users (id),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT outreach_dispatches_post_id_org_id_fkey FOREIGN KEY (post_id, org_id) REFERENCES posts (id, org_id),
  CONSTRAINT outreach_dispatches_segment_id_org_id_fkey FOREIGN KEY (segment_id, org_id) REFERENCES segments (id, org_id),
  CONSTRAINT outreach_dispatches_completed_chk CHECK (status <> 'completed' OR completed_at IS NOT NULL)
);
CREATE INDEX outreach_dispatches_org_idx ON outreach_dispatches (org_id, created_at DESC);
CREATE TRIGGER outreach_dispatches_touch_updated_at BEFORE UPDATE ON outreach_dispatches
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON outreach_dispatches TO app_rw;
COMMIT;
