-- 000148_create_terms_acceptances.up.sql (BE-118; user decision Q-17; 02 §4.8
-- doc patch). The terms of use acceptance ledger: which published
-- terms_of_use version (notice_versions, 02 §4.2) a user accepted, when,
-- through which client and in which request. Accepting the terms forms the
-- service contract (TBK); it is not a KVKK explicit consent, so it is kept
-- out of the consents ledger (only explicit_consent purposes go there,
-- Kurul 2026/347) and out of consent_current.
--
-- * Append-only like consents (000013): app_rw has SELECT/INSERT only and
--   koro_forbid_mutation blocks UPDATE/DELETE/TRUNCATE for everyone; the
--   only way out is terms_acceptances_purge below (000121 pattern).
-- * The composite FK names the notice, its kind and its body digest: an
--   acceptance can only point to a terms_of_use row, and text_sha256 is the
--   digest of exactly that row's body (06 H-38: derived from the parent, the
--   database refuses anything else). notice_versions gets the unique key the
--   FK needs (trivially unique: id is the primary key).
-- * One row per (user, version): accepting the same version again is a
--   no-op (the service inserts ON CONFLICT DO NOTHING).
-- * Least data: no IP or user agent (the audit row of the same request_id
--   carries them for 2 years, audit_events); request_id is the correlation.
-- * Retention terms_acceptance (02 §24.2 doc patch): kept while the account
--   lives (every version: which terms governed which period), then 3 years
--   after the account erasure (anchor withdrawn_or_erased, like
--   consent_proof); unapproved like every seeded row (T-35).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE notice_versions ADD CONSTRAINT notice_versions_id_kind_sha_uq UNIQUE (id, kind, body_sha256);
CREATE TABLE terms_acceptances (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id            uuid NOT NULL CONSTRAINT terms_acceptances_user_id_fkey REFERENCES users (id),
  notice_version_id  uuid NOT NULL,
  notice_kind        text NOT NULL DEFAULT 'terms_of_use' CONSTRAINT terms_acceptances_notice_kind_chk CHECK (notice_kind = 'terms_of_use'),
  text_sha256        bytea NOT NULL CONSTRAINT terms_acceptances_text_sha256_chk CHECK (octet_length(text_sha256) = 32),
  channel            text NOT NULL CONSTRAINT terms_acceptances_channel_chk CHECK (channel IN ('web','mobile')),
  request_id         text CONSTRAINT terms_acceptances_request_id_chk CHECK (request_id ~ '^[A-Za-z0-9_:-]{1,80}$'),
  accepted_at        timestamptz NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT terms_acceptances_notice_fkey FOREIGN KEY (notice_version_id, notice_kind, text_sha256)
    REFERENCES notice_versions (id, kind, body_sha256),
  CONSTRAINT terms_acceptances_user_notice_uq UNIQUE (user_id, notice_version_id)
);
CREATE INDEX terms_acceptances_notice_idx ON terms_acceptances (notice_version_id);
CREATE TRIGGER terms_acceptances_immutable BEFORE UPDATE OR DELETE ON terms_acceptances
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER terms_acceptances_no_truncate BEFORE TRUNCATE ON terms_acceptances
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON terms_acceptances TO app_rw;

-- The retention of the table (02 §24.2 doc patch; consent's sweeper).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('terms_acceptance', 'consent', 'terms_acceptances', '3 years', 'withdrawn_or_erased', 'hard_delete', 'Sözleşme ispatı (TBK); hukukçu onayı bekler', true,
   'Kullanım koşulları kabul kayıtları hesap yaşadıkça saklanır, hesap silindikten 3 yıl sonra silinir.')
ON CONFLICT (code) DO NOTHING;

-- terms_acceptances_purge (000121 pattern): the acceptances of the accounts
-- erased before the cut-off, oldest first, at most p_limit per call; a dry
-- run counts. The cut-off is least(p_cutoff, now() - the policy's period); a
-- real purge needs the policy enabled and legally approved (55000).
CREATE FUNCTION public.terms_acceptances_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('terms_acceptance', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
BEGIN
  IF p_dry_run THEN
    SELECT count(*) INTO v_n FROM public.terms_acceptances t JOIN public.users u ON u.id = t.user_id
     WHERE u.status = 'erased' AND u.erased_at < v_cutoff;
    RETURN v_n;
  END IF;
  v_ids := ARRAY(SELECT t.id FROM public.terms_acceptances t JOIN public.users u ON u.id = t.user_id
                  WHERE u.status = 'erased' AND u.erased_at < v_cutoff
                  ORDER BY t.accepted_at, t.id LIMIT p_limit);
  IF cardinality(v_ids) = 0 THEN
    RETURN 0;
  END IF;
  PERFORM set_config('koro.retention_purge', 'public.terms_acceptances', true);
  DELETE FROM public.terms_acceptances WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.terms_acceptances_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.terms_acceptances_purge(timestamptz, integer, boolean) TO app_rw;
COMMIT;
