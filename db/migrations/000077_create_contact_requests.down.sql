-- 000077_create_contact_requests.down.sql
-- DATA LOSS: every contact request.
BEGIN;
DROP TABLE IF EXISTS contact_requests;
COMMIT;
