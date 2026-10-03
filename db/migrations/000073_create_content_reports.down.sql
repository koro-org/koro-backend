-- 000073_create_content_reports.down.sql
-- DATA LOSS: every content report.
BEGIN;
DROP TABLE IF EXISTS content_reports;
COMMIT;
