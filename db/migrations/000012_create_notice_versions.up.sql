-- 000012_create_notice_versions.up.sql (02 §4.2). Versioned privacy notices
-- and consent texts. A new version is a new row; notice_versions_guard makes
-- a published row immutable (the only change left is retiring it, once) and
-- undeletable, because consents refer to the exact text that was shown.
-- Drafts stay editable. Versions are retired, never deleted, so app_rw has
-- no DELETE. created_by gets its FK in 000018 (after users).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE notice_versions (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  kind          text NOT NULL CONSTRAINT notice_versions_kind_chk CHECK (kind IN ('privacy_notice','consent_text','terms_of_use','cookie_notice')),
  purpose_code  text CONSTRAINT notice_versions_purpose_code_fkey REFERENCES consent_purposes (code),
  audience      text NOT NULL CONSTRAINT notice_versions_audience_chk CHECK (audience IN ('all','student','company','club','university')),
  locale        text NOT NULL DEFAULT 'tr-TR' CONSTRAINT notice_versions_locale_chk CHECK (locale IN ('tr-TR','en-US')),
  version       text NOT NULL CONSTRAINT notice_versions_version_chk CHECK (version ~ '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}(-[0-9]+)?$'), -- 2026.10.02
  title         text NOT NULL CONSTRAINT notice_versions_title_chk CHECK (char_length(title) <= 200),
  body_md       text NOT NULL,
  body_sha256   bytea NOT NULL CONSTRAINT notice_versions_body_sha256_chk CHECK (octet_length(body_sha256) = 32),
  published_at  timestamptz,
  retired_at    timestamptz,
  created_by    uuid,                -- FK users (000018)
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT notice_versions_purpose_chk CHECK ((kind = 'consent_text') = (purpose_code IS NOT NULL)),
  CONSTRAINT notice_versions_retired_chk CHECK (retired_at IS NULL OR published_at IS NOT NULL)
);
CREATE UNIQUE INDEX notice_versions_version_uq ON notice_versions (kind, coalesce(purpose_code, ''), audience, locale, version);
CREATE UNIQUE INDEX notice_versions_current_uq ON notice_versions (kind, coalesce(purpose_code, ''), audience, locale)
  WHERE published_at IS NOT NULL AND retired_at IS NULL;

CREATE FUNCTION public.notice_versions_guard() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.published_at IS NULL THEN
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE'
     AND OLD.retired_at IS NULL AND NEW.retired_at IS NOT NULL
     AND (to_jsonb(NEW) - 'retired_at') = (to_jsonb(OLD) - 'retired_at') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION '% on %.% is not allowed: a published notice version only gets retired_at set, once', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER notice_versions_guard BEFORE UPDATE OR DELETE ON notice_versions
  FOR EACH ROW EXECUTE FUNCTION public.notice_versions_guard();

GRANT SELECT, INSERT, UPDATE ON notice_versions TO app_rw;
COMMIT;
