-- 000095_create_legal_party_documents.down.sql
-- DATA LOSS: the evidence documents of every legal party (the files stay).
BEGIN;
DROP TABLE IF EXISTS legal_party_documents;
COMMIT;
