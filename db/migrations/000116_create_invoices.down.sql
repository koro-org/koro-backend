-- 000116_create_invoices.down.sql
-- DATA LOSS: every invoice record (the PDF files stay in files).
BEGIN;
DROP TABLE IF EXISTS invoices;
COMMIT;
