-- 000094_create_legal_parties.down.sql
-- DATA LOSS: every legal party (invoice titles, tax numbers, encrypted TCKNs).
BEGIN;
DROP TABLE IF EXISTS legal_parties;
COMMIT;
