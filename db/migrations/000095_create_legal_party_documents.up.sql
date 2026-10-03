-- 000095_create_legal_party_documents.up.sql (02 §17.2). Evidence documents
-- of a legal party (tax certificate, signature circular, foundation deed,
-- university protocol, ...); each file belongs to one document row. They are
-- financial records (financial_records, 10 years): app_rw has no DELETE, a
-- replaced document is a new row.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE legal_party_documents (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  legal_party_id  uuid NOT NULL CONSTRAINT legal_party_documents_legal_party_id_fkey REFERENCES legal_parties (id),
  kind            text NOT NULL CONSTRAINT legal_party_documents_kind_chk CHECK (kind IN ('tax_certificate','signature_circular','foundation_deed',
                    'university_protocol','authorization_letter','other')),
  file_id         uuid NOT NULL CONSTRAINT legal_party_documents_file_id_uq UNIQUE
                    CONSTRAINT legal_party_documents_file_id_fkey REFERENCES files (id),
  uploaded_by     uuid NOT NULL CONSTRAINT legal_party_documents_uploaded_by_fkey REFERENCES users (id),
  verified_at     timestamptz,
  verified_by     uuid CONSTRAINT legal_party_documents_verified_by_fkey REFERENCES users (id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT legal_party_documents_verified_chk CHECK ((verified_at IS NULL) = (verified_by IS NULL))
);
CREATE INDEX legal_party_documents_party_idx ON legal_party_documents (legal_party_id);
GRANT SELECT, INSERT, UPDATE ON legal_party_documents TO app_rw;
COMMIT;
