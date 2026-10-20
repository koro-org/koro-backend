-- 000126_event_export_format.up (BE-050; 02 §11.9 doc patch). The file
-- type an organiser chose for a registration export: the worker writes it
-- (files purpose event_export allows text/csv and xlsx). Existing rows
-- (none before BE-050's code) default to xlsx.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE event_data_exports ADD COLUMN format text NOT NULL DEFAULT 'xlsx'
  CONSTRAINT event_data_exports_format_chk CHECK (format IN ('csv','xlsx'));
COMMIT;
