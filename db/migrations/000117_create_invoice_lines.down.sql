-- 000117_create_invoice_lines.down.sql
-- DATA LOSS: the lines of every invoice.
BEGIN;
DROP TABLE IF EXISTS invoice_lines;
COMMIT;
