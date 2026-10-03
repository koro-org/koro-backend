-- 000013_create_consents.up.sql (02 §4.3). Append-only consent ledger: a
-- withdrawal is a new row. Two layers (02 §0.5): app_rw has SELECT/INSERT
-- only, and koro_forbid_mutation blocks UPDATE/DELETE/TRUNCATE for everyone,
-- the owner included. user_id gets its FK in 000017 (after users).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE consents (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id            uuid NOT NULL,          -- FK users (000017)
  purpose_code       text NOT NULL CONSTRAINT consents_purpose_code_fkey REFERENCES consent_purposes (code),
  action             text NOT NULL CONSTRAINT consents_action_chk CHECK (action IN ('granted','withdrawn')),
  notice_version_id  uuid CONSTRAINT consents_notice_version_id_fkey REFERENCES notice_versions (id),
  text_sha256        bytea CONSTRAINT consents_text_sha256_chk CHECK (octet_length(text_sha256) = 32),   -- digest of the shown text
  scope_type         text NOT NULL DEFAULT 'none' CONSTRAINT consents_scope_type_chk CHECK (scope_type IN ('none','registration')),
  scope_id           uuid,
  channel            text NOT NULL CONSTRAINT consents_channel_chk CHECK (channel IN ('web','mobile','email_link','admin','import')),
  ip                 inet,
  user_agent         text CONSTRAINT consents_user_agent_chk CHECK (char_length(user_agent) <= 400),
  occurred_at        timestamptz NOT NULL DEFAULT now(),
  created_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT consents_proof_chk CHECK (action = 'withdrawn' OR (notice_version_id IS NOT NULL AND text_sha256 IS NOT NULL)),
  CONSTRAINT consents_scope_chk CHECK ((scope_type = 'none') = (scope_id IS NULL))
);
CREATE INDEX consents_current_idx ON consents (user_id, purpose_code, scope_id, occurred_at DESC, id DESC);
CREATE INDEX consents_purpose_idx ON consents (purpose_code, occurred_at);
CREATE TRIGGER consents_immutable BEFORE UPDATE OR DELETE ON consents
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER consents_no_truncate BEFORE TRUNCATE ON consents
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON consents TO app_rw;
COMMIT;
