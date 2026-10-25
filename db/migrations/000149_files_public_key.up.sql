-- 000149_files_public_key.up.sql (BE-120; 02 §3.1 doc patch). Public media:
-- the cleared org logos and event covers get a permanent, cacheable copy in
-- the public media bucket (S3_BUCKET_PUBLIC) under a content-addressed key
-- "<purpose>/<file id>-<first 16 bytes of the SHA-256, hex><ext>" (a new
-- image is a new file, so a new URL; the copy is served with an immutable
-- Cache-Control). The owning module decides which records may show it.
--   * files.public_key: the key of the public copy, NULL while there is none.
--   * files_public_key_chk: only a publishable purpose of an active file
--     whose scan cleared it (clean, or skipped while FILES_SCAN_REQUIRED is
--     false) carries one; an infected verdict clears it in the same update.
--     One CHECK per rule (06 H-37: no "(state) = (A AND B)").
--   * files_public_key_format_chk: the server's key shape, never a name.
--   * files_public_key_uq: one public object per key.
--   * files_unpublished_idx: the files.publish_public backfill reads it.
--     Built in this transaction like 000137's index: before the pilot the
--     table is small.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE files ADD COLUMN public_key text;
ALTER TABLE files ADD CONSTRAINT files_public_key_chk CHECK (
  public_key IS NULL OR (purpose IN ('org_logo','event_cover') AND status = 'active' AND scan_status IN ('clean','skipped')));
ALTER TABLE files ADD CONSTRAINT files_public_key_format_chk CHECK (
  public_key IS NULL OR public_key ~ '^(org_logo|event_cover)/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-[0-9a-f]{32}\.(jpg|png|webp)$');
ALTER TABLE files ADD CONSTRAINT files_public_key_uq UNIQUE (public_key);
CREATE INDEX files_unpublished_idx ON files (id)
  WHERE public_key IS NULL AND purpose IN ('org_logo','event_cover') AND status = 'active';
COMMIT;
