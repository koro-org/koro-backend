-- 000126_event_export_format.down
-- DATA LOSS: the chosen file type of the registration exports (the files
-- themselves are kept; a queued export is then built as xlsx).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE event_data_exports DROP COLUMN format;
COMMIT;
