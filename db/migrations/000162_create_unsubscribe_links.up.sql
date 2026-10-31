-- 000162_create_unsubscribe_links.up.sql (BE-153; user decision Q-36; 02 §4.9
-- doc patch). The opaque one-click unsubscribe links of commercial e-mail.
-- Until BE-153 a link carried a signed token holding the recipient's user ID
-- in clear (BE-062, stateless): whoever saw the URL (an access log, the
-- browser history, a forwarded mail) could read the ID. A link is now
-- <CONSENT_UNSUBSCRIBE_BASE_URL>/<32 random bytes, base64url>, minted for
-- every commercial e-mail; this table maps the token to its recipient.
-- * Only the SHA-256 of the token is stored (unique; the 05 §14.2 pattern of
--   sessions and e-mail tokens): the database never holds a working link.
-- * purpose_code: only commercial_email has a link (push is refused in the
--   app, SMS is not used).
-- * A link is not single use: every click until expires_at withdraws the
--   consent in force (a mailbox provider retries; a user who granted again
--   and clicks an older mail must still be able to refuse).
-- * KVKK: personal (the user ID). consent's provider deletes the links of
--   an erased account; they are not exported (a digest and a purpose only;
--   the e-mails themselves are in notify's export). Retention
--   unsubscribe_link: the daily consent.unsubscribe_links_cleanup deletes a
--   link 7 days after its expiry (operational, never deferred to the legal
--   approval; seeded here, unapproved like every seeded row, T-35).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE unsubscribe_links (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  token_hash    bytea NOT NULL CONSTRAINT unsubscribe_links_token_hash_uq UNIQUE
                  CONSTRAINT unsubscribe_links_token_hash_chk CHECK (octet_length(token_hash) = 32),
  user_id       uuid NOT NULL CONSTRAINT unsubscribe_links_user_id_fkey REFERENCES users (id),
  purpose_code  text NOT NULL CONSTRAINT unsubscribe_links_purpose_code_chk CHECK (purpose_code = 'commercial_email'),
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  CONSTRAINT unsubscribe_links_expiry_chk CHECK (expires_at > created_at)
);
CREATE INDEX unsubscribe_links_user_idx ON unsubscribe_links (user_id);
CREATE INDEX unsubscribe_links_expires_at_idx ON unsubscribe_links (expires_at);
GRANT SELECT, INSERT, DELETE ON unsubscribe_links TO app_rw;

-- The retention of the table (02 §24.2 doc patch; a job policy like
-- email_token: consent.unsubscribe_links_cleanup applies the period).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('unsubscribe_link', 'consent', 'unsubscribe_links', '7 days', 'expires_at', 'hard_delete', 'Operasyonel (veri minimizasyonu)', true,
   'Abonelikten çıkma bağlantıları (belirteç özeti, kullanıcı kimliği, amaç) süresi dolduktan 7 gün sonra, hesap silinince hemen silinir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
