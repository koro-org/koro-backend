-- 000066_create_posts.up.sql (02 §12.2). A post (announcement) of an org.
-- Visibility is decided at read time from audience_keys (02 §9.1); the
-- CHECKs pin the shape of each kind, the audience and the status:
--   * a company post is always commercial;
--   * a company post to a segment needs an audience estimate of at least 10
--     and the consumed entitlement (posts_company_segment_chk);
--   * published/hidden carry published_at, removed carries removed_at.
-- 02 fixes (H-20):
--   * posts_company_segment_chk: `audience_size_estimate >= 10` is NULL for a
--     missing estimate, which made the whole CHECK NULL (= passed); the
--     estimate must now be present (IS NOT NULL AND >= 10).
--   * the audience index is posts_audience_idx (02 §23 naming), not _gin.
--   * posts_id_org_uq (06 H-38): the target of the composite FKs of
--     outreach_dispatches, so a dispatch belongs to the org of its post.
--   * posts_touch_updated_at skips counter-only updates (BE-029, below).
-- Removed posts are hard-deleted 12 months later with their media and
-- interactions (ON DELETE CASCADE on the child tables).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE posts (
  id                      uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id                  uuid NOT NULL,                                    -- the author org
  author_org_kind         text NOT NULL CONSTRAINT posts_author_org_kind_chk CHECK (author_org_kind IN ('platform','university','club','company')),
  author_user_id          uuid NOT NULL CONSTRAINT posts_author_user_id_fkey REFERENCES users (id),   -- who posted (audit; readers see the org)
  kind                    text NOT NULL CONSTRAINT posts_kind_chk CHECK (kind IN ('text','image','event_share','opportunity_call')),
  title                   text COLLATE "tr" CONSTRAINT posts_title_chk CHECK (char_length(title) <= 140),   -- notification / preview title
  body_json               jsonb,                                            -- validated Tiptap JSON
  body_text               text CONSTRAINT posts_body_text_chk CHECK (char_length(body_text) <= 5000),   -- plain text (preview / search)
  schema_version          smallint NOT NULL DEFAULT 1,
  event_id                uuid CONSTRAINT posts_event_id_fkey REFERENCES events (id),
  opportunity             jsonb,                                            -- R-003 §1.6 shape
  audience_kind           text NOT NULL CONSTRAINT posts_audience_kind_chk CHECK (audience_kind IN ('public','followers','segment')),
  audience_keys           text[] NOT NULL,                                  -- ['all'] | ['uni:..','year:3',...]
  audience_dim_count      smallint NOT NULL CONSTRAINT posts_audience_dim_count_chk CHECK (audience_dim_count BETWEEN 1 AND 4),
  audience_snapshot       jsonb,                                            -- segment definition at publication (audit)
  audience_size_estimate  integer,
  commercial_flag         boolean NOT NULL DEFAULT false,                   -- "Tanıtım" label
  status                  text NOT NULL DEFAULT 'draft'
                            CONSTRAINT posts_status_chk CHECK (status IN ('draft','pending_review','published','hidden','removed')),
  published_at            timestamptz,
  edited_at               timestamptz,
  reviewed_by             uuid CONSTRAINT posts_reviewed_by_fkey REFERENCES users (id),
  reviewed_at             timestamptz,
  removed_at              timestamptz,
  like_count              integer NOT NULL DEFAULT 0 CONSTRAINT posts_like_count_chk CHECK (like_count >= 0),
  save_count              integer NOT NULL DEFAULT 0 CONSTRAINT posts_save_count_chk CHECK (save_count >= 0),
  entitlement_usage_id    uuid CONSTRAINT posts_entitlement_usage_id_fkey REFERENCES entitlement_usages (id),
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now(),
  search_vector           tsvector GENERATED ALWAYS AS
                            (to_tsvector('public.koro_tr'::regconfig, coalesce(title, '') || ' ' || coalesce(body_text, ''))) STORED,
  CONSTRAINT posts_org_id_author_org_kind_fkey FOREIGN KEY (org_id, author_org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT posts_id_org_uq UNIQUE (id, org_id),
  CONSTRAINT posts_event_share_chk CHECK ((kind = 'event_share') = (event_id IS NOT NULL)),
  CONSTRAINT posts_opportunity_chk CHECK ((kind = 'opportunity_call') = (opportunity IS NOT NULL)),
  CONSTRAINT posts_body_chk CHECK (kind IN ('image','event_share') OR body_json IS NOT NULL),
  CONSTRAINT posts_company_commercial_chk CHECK (author_org_kind <> 'company' OR commercial_flag),
  CONSTRAINT posts_audience_all_chk CHECK (audience_kind = 'segment' OR (audience_keys = ARRAY['all']::text[] AND audience_dim_count = 1)),
  CONSTRAINT posts_audience_snapshot_chk CHECK (audience_kind <> 'segment' OR audience_snapshot IS NOT NULL),
  CONSTRAINT posts_company_segment_chk CHECK (audience_kind <> 'segment' OR author_org_kind <> 'company'
         OR (audience_size_estimate IS NOT NULL AND audience_size_estimate >= 10 AND entitlement_usage_id IS NOT NULL)),
  CONSTRAINT posts_published_chk CHECK (status NOT IN ('published','hidden') OR published_at IS NOT NULL),
  CONSTRAINT posts_removed_chk CHECK ((status = 'removed') = (removed_at IS NOT NULL))
);
CREATE INDEX posts_feed_idx     ON posts (published_at DESC, id DESC) WHERE status = 'published';
CREATE INDEX posts_audience_idx ON posts USING gin (audience_keys) WHERE status = 'published';
CREATE INDEX posts_author_idx   ON posts (org_id, published_at DESC, id DESC) WHERE status = 'published';
CREATE INDEX posts_review_idx   ON posts (created_at) WHERE status = 'pending_review';
CREATE INDEX posts_removed_idx  ON posts (removed_at) WHERE status = 'removed';
CREATE INDEX posts_fts_idx      ON posts USING gin (search_vector) WHERE status = 'published';
-- A counter-only update (like_count / save_count; BE-029) keeps updated_at:
-- updated_at is the post's version (its ETag, 04 §1.13), and a like must
-- not make its author's If-Match stale.
CREATE TRIGGER posts_touch_updated_at BEFORE UPDATE ON posts
  FOR EACH ROW WHEN (OLD.like_count IS NOT DISTINCT FROM NEW.like_count AND OLD.save_count IS NOT DISTINCT FROM NEW.save_count)
  EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON posts TO app_rw;
COMMIT;
