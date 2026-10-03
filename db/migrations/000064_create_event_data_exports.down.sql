-- 000064_create_event_data_exports.down.sql
-- DATA LOSS: the export log (who exported which attendee data).
BEGIN;
DROP TABLE IF EXISTS event_data_exports;
COMMIT;
