-- 000174_create_message_reports.down.sql
-- DATA LOSS: every message report (the content_reports rows of target_type
-- 'message', their moderation history links) and the reported messages kept
-- with them. Roll back only together with the code that writes them.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE message_report_messages;
DROP TABLE message_reports;
DELETE FROM content_reports WHERE target_type = 'message';
ALTER TABLE content_reports
  DROP CONSTRAINT content_reports_id_target_uq,
  DROP CONSTRAINT content_reports_target_type_chk,
  ADD CONSTRAINT content_reports_target_type_chk CHECK (target_type IN ('post','org'));
COMMIT;
