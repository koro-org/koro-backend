-- 000064_create_event_data_exports.up.sql (02 §11.9). The log and file handle
-- of an attendee-list export: who asked, why, which fields, from which IP.
-- The file lives 7 days (expires_at, status 'purged' clears file_id), the
-- record 2 years (retention 'event_export_log'). include_sensitive stays
-- false in v1 (only a platform approval may set it; application rule).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE event_data_exports (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  event_id             uuid NOT NULL CONSTRAINT event_data_exports_event_id_fkey REFERENCES events (id),
  org_id               uuid NOT NULL CONSTRAINT event_data_exports_org_id_fkey REFERENCES organizations (id),
  requested_by         uuid NOT NULL CONSTRAINT event_data_exports_requested_by_fkey REFERENCES users (id),
  reason               text NOT NULL CONSTRAINT event_data_exports_reason_chk CHECK (char_length(reason) BETWEEN 10 AND 500),
  fields               text[] NOT NULL CONSTRAINT event_data_exports_fields_chk CHECK (cardinality(fields) BETWEEN 1 AND 40),
  include_sensitive    boolean NOT NULL DEFAULT false,
  status               text NOT NULL DEFAULT 'queued' CONSTRAINT event_data_exports_status_chk CHECK (status IN ('queued','ready','failed','purged')),
  row_count            integer CONSTRAINT event_data_exports_row_count_chk CHECK (row_count >= 0),
  file_id              uuid CONSTRAINT event_data_exports_file_id_fkey REFERENCES files (id),
  file_sha256          bytea CONSTRAINT event_data_exports_file_sha256_chk CHECK (octet_length(file_sha256) = 32),
  ip                   inet,
  expires_at           timestamptz NOT NULL,                -- created_at + 7 days
  first_downloaded_at  timestamptz,
  download_count       smallint NOT NULL DEFAULT 0 CONSTRAINT event_data_exports_download_count_chk CHECK (download_count >= 0),
  purged_at            timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT event_data_exports_ready_chk CHECK (status <> 'ready' OR file_id IS NOT NULL),
  CONSTRAINT event_data_exports_purged_chk CHECK ((status = 'purged') = (purged_at IS NOT NULL)),
  CONSTRAINT event_data_exports_purged_file_chk CHECK (status <> 'purged' OR file_id IS NULL)
);
CREATE INDEX event_data_exports_event_idx  ON event_data_exports (event_id, created_at DESC);
CREATE INDEX event_data_exports_expiry_idx ON event_data_exports (expires_at) WHERE status = 'ready';
CREATE TRIGGER event_data_exports_touch_updated_at BEFORE UPDATE ON event_data_exports
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON event_data_exports TO app_rw;
COMMIT;
