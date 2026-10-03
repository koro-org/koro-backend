-- 000062_create_registration_tickets.down.sql
-- DATA LOSS: every issued ticket (the QR tokens of all registrations).
BEGIN;
DROP TABLE IF EXISTS registration_tickets;
COMMIT;
