-- 000009_create_files.up.sql (02 §3.1). Uploaded object metadata; the
-- per-purpose MIME/size allow-list lives in the files module (BE-008).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE files (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  purpose             text NOT NULL CONSTRAINT files_purpose_chk CHECK (purpose IN (
                        'avatar','cv','edu_document','org_logo','event_cover','post_media',
                        'club_claim_evidence','party_document','bank_account_evidence',
                        'transfer_receipt','payout_receipt','refund_receipt',
                        'invoice_pdf','event_export','dsr_export')),
  owner_user_id       uuid,          -- no FK: files is the lowest layer; ownership is decided by the owning module
  org_id              uuid,          -- no FK (same reason)
  attached_module     text CONSTRAINT files_attached_module_chk CHECK (attached_module ~ '^[a-z_]+$'),
  attached_entity_id  uuid,
  attached_at         timestamptz,
  bucket              text NOT NULL,
  storage_key         text NOT NULL CONSTRAINT files_storage_key_uq UNIQUE, -- "<purpose>/<yyyy>/<mm>/<id>"; never the original name
  original_name       text CONSTRAINT files_original_name_chk CHECK (char_length(original_name) <= 255),
  declared_mime       text NOT NULL,
  detected_mime       text,                          -- magic bytes
  size_bytes          bigint CONSTRAINT files_size_bytes_chk CHECK (size_bytes BETWEEN 1 AND 104857600),
  sha256              bytea CONSTRAINT files_sha256_chk CHECK (octet_length(sha256) = 32),
  status              text NOT NULL DEFAULT 'uploading' CONSTRAINT files_status_chk CHECK (status IN ('uploading','active','rejected')), -- rejected: BE-059 metadata removal failed
  scan_status         text NOT NULL DEFAULT 'pending' CONSTRAINT files_scan_status_chk CHECK (scan_status IN ('pending','clean','infected','skipped')),
  scanned_at          timestamptz,
  uploaded_by         uuid,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT files_attached_chk CHECK ((attached_module IS NULL) = (attached_entity_id IS NULL)),
  CONSTRAINT files_active_chk   CHECK (status = 'uploading' OR (sha256 IS NOT NULL AND size_bytes IS NOT NULL))
);
CREATE INDEX files_owner_idx    ON files (owner_user_id) WHERE owner_user_id IS NOT NULL;
CREATE INDEX files_org_idx      ON files (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX files_attached_idx ON files (attached_module, attached_entity_id);
CREATE INDEX files_orphan_idx   ON files (created_at) WHERE attached_at IS NULL;
CREATE INDEX files_scan_idx     ON files (created_at) WHERE scan_status = 'pending';
CREATE TRIGGER files_touch_updated_at BEFORE UPDATE ON files
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON files TO app_rw;
COMMIT;
