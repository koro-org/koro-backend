-- 000013_create_consents.down.sql
-- DATA LOSS: the whole consent ledger (the proof of consent, 6563 / KVKK).
BEGIN;
DROP TABLE IF EXISTS consents;
COMMIT;
