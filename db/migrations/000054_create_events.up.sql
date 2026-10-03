-- 000054_create_events.up.sql (02 §11.1). The event. Capacity is protected
-- twice: the conditional UPDATE of confirmed_count in the service and the
-- CHECK events_capacity_fill_chk here. Retention is unlimited and removal is
-- a status ('cancelled'); only a draft may be deleted, which the BEFORE
-- DELETE trigger enforces (a published event leaves registrations, tickets
-- and an immutable form behind). 02 §0.4: online_url is https only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE events (
  id                        uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id                    uuid NOT NULL CONSTRAINT events_org_id_fkey REFERENCES organizations (id),   -- organiser (club / university / platform; companies are closed in v1)
  created_by                uuid NOT NULL CONSTRAINT events_created_by_fkey REFERENCES users (id),
  title                     text COLLATE public.tr NOT NULL CONSTRAINT events_title_chk CHECK (char_length(title) BETWEEN 3 AND 160),
  slug                      text NOT NULL CONSTRAINT events_slug_chk CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) <= 100),
  summary                   text CONSTRAINT events_summary_chk CHECK (char_length(summary) <= 300),
  description_json          jsonb,                                         -- validated Tiptap JSON (feed allow-list)
  description_text          text CONSTRAINT events_description_text_chk CHECK (char_length(description_text) <= 20000),
  category                  text NOT NULL CONSTRAINT events_category_chk CHECK (category IN ('workshop','talk','competition','career','social',
                              'trip','sports','culture','volunteering','other')),
  format                    text NOT NULL CONSTRAINT events_format_chk CHECK (format IN ('in_person','online','hybrid')),
  location_name             text CONSTRAINT events_location_name_chk CHECK (char_length(location_name) <= 200),
  address                   text CONSTRAINT events_address_chk CHECK (char_length(address) <= 500),
  city                      text COLLATE public.tr CONSTRAINT events_city_chk CHECK (char_length(city) <= 60),
  online_url                text CONSTRAINT events_online_url_chk CHECK (online_url ~ '^https://' AND char_length(online_url) <= 2048),   -- shown to approved attendees only
  starts_at                 timestamptz NOT NULL,
  ends_at                   timestamptz NOT NULL,
  cover_file_id             uuid CONSTRAINT events_cover_file_id_fkey REFERENCES files (id),
  visibility                text NOT NULL DEFAULT 'public' CONSTRAINT events_visibility_chk CHECK (visibility IN ('public','university','members')),
  visibility_university_id  uuid CONSTRAINT events_visibility_university_id_fkey REFERENCES universities (id),
  status                    text NOT NULL DEFAULT 'draft' CONSTRAINT events_status_chk CHECK (status IN ('draft','published','cancelled','completed')),
  published_at              timestamptz,
  cancelled_at              timestamptz,
  cancel_reason             text CONSTRAINT events_cancel_reason_chk CHECK (char_length(cancel_reason) <= 500),
  requires_registration     boolean NOT NULL DEFAULT true,
  registration_mode         text NOT NULL DEFAULT 'auto' CONSTRAINT events_registration_mode_chk CHECK (registration_mode IN ('auto','manual')),   -- manual: the UI says "Apply"
  capacity                  integer CONSTRAINT events_capacity_chk CHECK (capacity > 0),
  confirmed_count           integer NOT NULL DEFAULT 0 CONSTRAINT events_confirmed_count_chk CHECK (confirmed_count >= 0),   -- confirmed + checked_in
  waitlist_enabled          boolean NOT NULL DEFAULT false,
  registration_opens_at     timestamptz,
  registration_closes_at    timestamptz,
  cancel_deadline_at        timestamptz,
  checkin_enabled           boolean NOT NULL DEFAULT false,
  attendance_finalized_at   timestamptz,
  attendance_finalized_by   uuid CONSTRAINT events_attendance_finalized_by_fkey REFERENCES users (id),   -- NULL with finalized_at set = the system
  is_sensitive_category     boolean NOT NULL DEFAULT false,      -- religious / political etc.: produces no talent signal
  created_at                timestamptz NOT NULL DEFAULT now(),
  updated_at                timestamptz NOT NULL DEFAULT now(),
  search_vector             tsvector GENERATED ALWAYS AS
                              (to_tsvector('public.koro_tr'::regconfig, coalesce(title, '') || ' ' || coalesce(summary, '') || ' ' || coalesce(description_text, ''))) STORED,
  CONSTRAINT events_org_slug_uq UNIQUE (org_id, slug),
  CONSTRAINT events_period_chk CHECK (ends_at > starts_at),
  CONSTRAINT events_capacity_fill_chk CHECK (capacity IS NULL OR confirmed_count <= capacity),
  CONSTRAINT events_visibility_university_chk CHECK ((visibility = 'university') = (visibility_university_id IS NOT NULL)),
  CONSTRAINT events_published_chk CHECK (status = 'draft' OR published_at IS NOT NULL),
  CONSTRAINT events_cancelled_chk CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
  CONSTRAINT events_registration_window_chk CHECK (registration_opens_at IS NULL OR registration_closes_at IS NULL OR registration_closes_at > registration_opens_at),
  CONSTRAINT events_registration_close_chk CHECK (registration_closes_at IS NULL OR registration_closes_at <= ends_at),
  CONSTRAINT events_online_url_required_chk CHECK (format = 'in_person' OR status = 'draft' OR online_url IS NOT NULL),
  CONSTRAINT events_registration_required_chk CHECK (requires_registration OR (capacity IS NULL AND NOT waitlist_enabled AND NOT checkin_enabled)),
  CONSTRAINT events_waitlist_capacity_chk CHECK (NOT waitlist_enabled OR capacity IS NOT NULL),
  CONSTRAINT events_attendance_chk CHECK (attendance_finalized_at IS NULL OR checkin_enabled)
);
CREATE INDEX events_org_idx     ON events (org_id, starts_at DESC);
CREATE INDEX events_listing_idx ON events (starts_at, id) WHERE status = 'published';
CREATE INDEX events_fts_idx     ON events USING gin (search_vector);
CREATE INDEX events_trgm_idx    ON events USING gin (public.koro_norm(title) public.gin_trgm_ops);
CREATE TRIGGER events_touch_updated_at BEFORE UPDATE ON events
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();

-- Only a draft may be deleted (02 §11.1 "taslak hard delete serbest").
CREATE FUNCTION public.events_forbid_nondraft_delete() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'DELETE on %.% is not allowed for a % event: only a draft may be deleted', TG_TABLE_SCHEMA, TG_TABLE_NAME, OLD.status
    USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER events_draft_delete_only BEFORE DELETE ON events
  FOR EACH ROW WHEN (OLD.status <> 'draft') EXECUTE FUNCTION public.events_forbid_nondraft_delete();
GRANT SELECT, INSERT, UPDATE, DELETE ON events TO app_rw;
COMMIT;
