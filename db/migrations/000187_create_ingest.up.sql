-- 000187_create_ingest.up.sql (BE-210; user decision 2026-11-11, koro-docs#11;
-- docs/research/kulup-etkinlik-toplama-2026-11-11.md). The ingest module: the
-- official club lists of the universities and the events of the clubs' own
-- pages, collected by deterministic fetchers and a structured-output
-- extractor, shown as "Otomatik toplandı · kaynak" until the club claims its
-- profile.
-- * ingest_sources: one fetched URL each (a club list, an events page, a
--   feed, a sitemap or a page a sitemap named: parent_id, fetched once with
--   interval_seconds 0). enabled starts false unless the environment's
--   default turns it on (dev/staging; prod after the lawyer's review); a
--   disabled source says why (paused_reason: admin, failures — fail_count
--   reached the limit, claimed — the club took over its profile, robots —
--   robots.txt refused the URL). The conditional GET state (etag,
--   last_modified) and the SHA-256 of the last body: an unchanged body is
--   never sent to the extractor. No page text is stored.
-- * ingest_runs: one row per fetch (outcome, counts, tokens); operational,
--   no personal data, deleted after 90 days by ingest.purge_expired.
-- * imported_clubs: the clubs seen on an official list (the club org is
--   directory's; the row keeps the matching key, the university's own
--   category label, needs_review when that label maps to a special category
--   or to nothing — the club then starts as "other" — the club's group
--   e-mail address on the university's domain, and missing_since when the
--   list dropped the club).
-- * imported_events: the events found, one per club (or university), day and
--   normalised title (fingerprint): olgusal fields only (title, time, place
--   text, category, source_url) with the evidence quote the extractor had to
--   cite from the page; no person, no contact, no image. hidden by a platform
--   admin or by an anonymous removal request; promoted when the club's admin
--   made it a draft event; rejected by the club's admin. expires_at is the
--   event's end plus the imported_event retention: ingest.purge_expired
--   deletes the row then (operational, never deferred to the legal approval;
--   seeded unapproved like every policy, T-35).
BEGIN;
SET LOCAL lock_timeout = '5s';

CREATE TABLE ingest_sources (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  kind             text NOT NULL CONSTRAINT ingest_sources_kind_chk CHECK (kind IN ('club_list','events')),
  adapter          text NOT NULL CONSTRAINT ingest_sources_adapter_chk CHECK (adapter IN (
                     'htmlpage','jsonapi','rss','sitemap','instagram_graph','manual_paste',
                     'clublist_ytu','clublist_boun','clublist_sabanci','clublist_metu','clublist_itu')),
  url              text NOT NULL CONSTRAINT ingest_sources_url_chk CHECK (url ~ '^https?://[^/?#@[:space:]]+' AND char_length(url) <= 2048),
  host             text NOT NULL CONSTRAINT ingest_sources_host_chk CHECK (host ~ '^[a-z0-9.-]{1,253}$'),
  label            text NOT NULL CONSTRAINT ingest_sources_label_chk CHECK (char_length(label) BETWEEN 1 AND 120),
  university_id    uuid NOT NULL CONSTRAINT ingest_sources_university_id_fkey REFERENCES universities (id),
  club_org_id      uuid CONSTRAINT ingest_sources_club_org_id_fkey REFERENCES clubs (org_id),
  parent_id        uuid CONSTRAINT ingest_sources_parent_id_fkey REFERENCES ingest_sources (id) ON DELETE CASCADE,
  interval_seconds integer NOT NULL CONSTRAINT ingest_sources_interval_chk CHECK (interval_seconds = 0 OR interval_seconds BETWEEN 3600 AND 2592000),
  enabled          boolean NOT NULL DEFAULT false,
  paused_reason    text CONSTRAINT ingest_sources_paused_reason_chk CHECK (paused_reason IN ('admin','failures','claimed','robots')),
  next_run_at      timestamptz,
  etag             text CONSTRAINT ingest_sources_etag_chk CHECK (char_length(etag) BETWEEN 1 AND 200),
  last_modified    text CONSTRAINT ingest_sources_last_modified_chk CHECK (char_length(last_modified) BETWEEN 1 AND 100),
  content_hash     bytea CONSTRAINT ingest_sources_content_hash_chk CHECK (octet_length(content_hash) = 32),
  fail_count       integer NOT NULL DEFAULT 0 CONSTRAINT ingest_sources_fail_count_chk CHECK (fail_count >= 0),
  zero_yield_runs  integer NOT NULL DEFAULT 0 CONSTRAINT ingest_sources_zero_yield_chk CHECK (zero_yield_runs >= 0),
  last_error       text CONSTRAINT ingest_sources_last_error_chk CHECK (char_length(last_error) BETWEEN 1 AND 64),
  last_status      smallint CONSTRAINT ingest_sources_last_status_chk CHECK (last_status BETWEEN 100 AND 599),
  last_yield       integer CONSTRAINT ingest_sources_last_yield_chk CHECK (last_yield >= 0),
  last_fetched_at  timestamptz,
  last_ok_at       timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ingest_sources_url_uq UNIQUE (url),
  CONSTRAINT ingest_sources_enabled_unpaused_chk CHECK (NOT enabled OR paused_reason IS NULL),
  CONSTRAINT ingest_sources_parent_kind_chk CHECK (parent_id IS NULL OR kind = 'events')
);
CREATE INDEX ingest_sources_due_idx ON ingest_sources (next_run_at) WHERE enabled;
CREATE INDEX ingest_sources_host_idx ON ingest_sources (host, last_fetched_at);
CREATE INDEX ingest_sources_university_idx ON ingest_sources (university_id);
CREATE INDEX ingest_sources_club_idx ON ingest_sources (club_org_id) WHERE club_org_id IS NOT NULL;
CREATE INDEX ingest_sources_parent_idx ON ingest_sources (parent_id) WHERE parent_id IS NOT NULL;
CREATE TRIGGER ingest_sources_touch_updated_at BEFORE UPDATE ON ingest_sources
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON ingest_sources TO app_rw;

CREATE TABLE ingest_runs (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  source_id     uuid NOT NULL CONSTRAINT ingest_runs_source_id_fkey REFERENCES ingest_sources (id) ON DELETE CASCADE,
  started_at    timestamptz NOT NULL,
  finished_at   timestamptz NOT NULL,
  outcome       text NOT NULL CONSTRAINT ingest_runs_outcome_chk CHECK (outcome IN (
                  'ok','unchanged','not_modified','robots_denied','blocked','http_error','fetch_error',
                  'parse_error','extract_error','unsupported')),
  http_status   smallint CONSTRAINT ingest_runs_http_status_chk CHECK (http_status BETWEEN 100 AND 599),
  bytes         integer NOT NULL DEFAULT 0 CONSTRAINT ingest_runs_bytes_chk CHECK (bytes >= 0),
  found         integer NOT NULL DEFAULT 0 CONSTRAINT ingest_runs_found_chk CHECK (found >= 0),
  created       integer NOT NULL DEFAULT 0 CONSTRAINT ingest_runs_created_chk CHECK (created >= 0),
  updated       integer NOT NULL DEFAULT 0 CONSTRAINT ingest_runs_updated_chk CHECK (updated >= 0),
  rejected      integer NOT NULL DEFAULT 0 CONSTRAINT ingest_runs_rejected_chk CHECK (rejected >= 0),
  input_tokens  integer NOT NULL DEFAULT 0 CONSTRAINT ingest_runs_input_tokens_chk CHECK (input_tokens >= 0),
  output_tokens integer NOT NULL DEFAULT 0 CONSTRAINT ingest_runs_output_tokens_chk CHECK (output_tokens >= 0),
  error_code    text CONSTRAINT ingest_runs_error_code_chk CHECK (char_length(error_code) BETWEEN 1 AND 64),
  CONSTRAINT ingest_runs_finished_chk CHECK (finished_at >= started_at)
);
CREATE INDEX ingest_runs_source_idx ON ingest_runs (source_id, started_at DESC);
CREATE INDEX ingest_runs_started_idx ON ingest_runs (started_at);
GRANT SELECT, INSERT, DELETE ON ingest_runs TO app_rw;

CREATE TABLE imported_clubs (
  club_org_id     uuid PRIMARY KEY CONSTRAINT imported_clubs_club_org_id_fkey REFERENCES clubs (org_id),
  university_id   uuid NOT NULL CONSTRAINT imported_clubs_university_id_fkey REFERENCES universities (id),
  source_id       uuid CONSTRAINT imported_clubs_source_id_fkey REFERENCES ingest_sources (id) ON DELETE SET NULL,
  name_key        text NOT NULL CONSTRAINT imported_clubs_name_key_chk CHECK (char_length(name_key) BETWEEN 1 AND 200),
  source_category text CONSTRAINT imported_clubs_source_category_chk CHECK (char_length(source_category) BETWEEN 1 AND 100),
  needs_review    boolean NOT NULL DEFAULT false,
  contact_email   text CONSTRAINT imported_clubs_contact_email_chk CHECK (
                    contact_email ~ '^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$' AND char_length(contact_email) <= 254),
  first_seen_at   timestamptz NOT NULL,
  last_seen_at    timestamptz NOT NULL,
  missing_since   timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT imported_clubs_name_uq UNIQUE (university_id, name_key)
);
CREATE INDEX imported_clubs_source_idx ON imported_clubs (source_id);
CREATE INDEX imported_clubs_review_idx ON imported_clubs (university_id) WHERE needs_review;
CREATE TRIGGER imported_clubs_touch_updated_at BEFORE UPDATE ON imported_clubs
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON imported_clubs TO app_rw;

CREATE TABLE imported_events (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  source_id         uuid CONSTRAINT imported_events_source_id_fkey REFERENCES ingest_sources (id) ON DELETE SET NULL,
  university_id     uuid NOT NULL CONSTRAINT imported_events_university_id_fkey REFERENCES universities (id),
  club_org_id       uuid CONSTRAINT imported_events_club_org_id_fkey REFERENCES clubs (org_id),
  status            text NOT NULL DEFAULT 'imported' CONSTRAINT imported_events_status_chk CHECK (status IN ('imported','hidden','promoted','rejected')),
  hidden_reason     text CONSTRAINT imported_events_hidden_reason_chk CHECK (hidden_reason IN ('admin','removal_request')),
  title             text NOT NULL CONSTRAINT imported_events_title_chk CHECK (char_length(title) BETWEEN 3 AND 200),
  starts_at         timestamptz NOT NULL,
  ends_at           timestamptz,
  all_day           boolean NOT NULL DEFAULT false,
  location_text     text CONSTRAINT imported_events_location_text_chk CHECK (char_length(location_text) BETWEEN 1 AND 200),
  category          text NOT NULL DEFAULT 'other' CONSTRAINT imported_events_category_chk CHECK (category IN (
                      'workshop','talk','competition','career','social','trip','sports','culture','volunteering','other')),
  source_url        text NOT NULL CONSTRAINT imported_events_source_url_chk CHECK (source_url ~ '^https?://' AND char_length(source_url) <= 2048),
  evidence          text NOT NULL CONSTRAINT imported_events_evidence_chk CHECK (char_length(evidence) BETWEEN 1 AND 500),
  confidence        numeric(3,2) NOT NULL CONSTRAINT imported_events_confidence_chk CHECK (confidence BETWEEN 0 AND 1),
  fingerprint       bytea NOT NULL CONSTRAINT imported_events_fingerprint_chk CHECK (octet_length(fingerprint) = 32),
  promoted_event_id uuid CONSTRAINT imported_events_promoted_event_id_fkey REFERENCES events (id) ON DELETE SET NULL,
  expires_at        timestamptz NOT NULL,
  last_seen_at      timestamptz NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT imported_events_fingerprint_uq UNIQUE (fingerprint),
  CONSTRAINT imported_events_ends_chk CHECK (ends_at IS NULL OR ends_at >= starts_at),
  CONSTRAINT imported_events_hidden_has_reason_chk CHECK (status <> 'hidden' OR hidden_reason IS NOT NULL),
  CONSTRAINT imported_events_reason_only_hidden_chk CHECK (status = 'hidden' OR hidden_reason IS NULL),
  CONSTRAINT imported_events_promoted_only_chk CHECK (status = 'promoted' OR promoted_event_id IS NULL),
  CONSTRAINT imported_events_expiry_chk CHECK (expires_at > starts_at)
);
CREATE INDEX imported_events_public_idx ON imported_events (starts_at) WHERE status = 'imported';
CREATE INDEX imported_events_university_idx ON imported_events (university_id, starts_at);
CREATE INDEX imported_events_club_idx ON imported_events (club_org_id, starts_at) WHERE club_org_id IS NOT NULL;
CREATE INDEX imported_events_expires_idx ON imported_events (expires_at);
CREATE INDEX imported_events_source_idx ON imported_events (source_id);
CREATE TRIGGER imported_events_touch_updated_at BEFORE UPDATE ON imported_events
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON imported_events TO app_rw;

-- The retention of the imported events (02 §24.2 doc patch; a job policy like
-- sms_code: ingest.purge_expired applies it, never deferred to the legal
-- approval because it only shortens what is kept).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('imported_event', 'ingest', 'imported_events', '7 days', 'event_end', 'hard_delete', 'Veri minimizasyonu (aleni kaynak, KVKK md. 4)', true,
   'Resmî kaynaklardan otomatik toplanan etkinlik kayıtları etkinlik bittikten 7 gün sonra silinir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
