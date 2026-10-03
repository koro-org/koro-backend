-- 000002_create_functions.up.sql (02 §0.3, §0.5; 01 §1.4). Helper functions
-- shared by every later migration. Object references are schema-qualified
-- so that index expressions also work under an empty search_path
-- (pg_dump/pg_restore).
BEGIN;
SET LOCAL lock_timeout = '5s';

-- koro_norm folds case with Turkish rules (I→ı, İ→i) and strips accents:
-- "İSTANBUL" → "istanbul", "IŞIK" → "isik", "ÇAĞLA" → "cagla". Used by the
-- trigram indexes of searchable tables. IMMUTABLE is a promise that holds
-- only while the ICU and unaccent rules do not change: after an ICU major
-- upgrade every koro_norm index is rebuilt (REINDEX; see the T-41 note).
CREATE FUNCTION public.koro_norm(t text) RETURNS text
  LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
  AS $$ SELECT public.unaccent('public.unaccent'::regdictionary, lower(t COLLATE public.tr)) $$;

-- [ts] columns: BEFORE UPDATE trigger that stamps updated_at.
CREATE FUNCTION public.koro_touch_updated_at() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END $$;

-- Append-only tables: BEFORE UPDATE OR DELETE (row) and BEFORE TRUNCATE
-- (statement) trigger. The second layer next to the missing UPDATE/DELETE
-- grants of app_rw (02 §0.5).
CREATE FUNCTION public.koro_forbid_mutation() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on %.% is not allowed: the table is append-only', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END $$;

-- commerce_policies variant: the only allowed change is closing an open row
-- (valid_to NULL → value) with every other column unchanged.
CREATE FUNCTION public.koro_policy_close_only() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE'
     AND OLD.valid_to IS NULL AND NEW.valid_to IS NOT NULL
     AND (to_jsonb(NEW) - 'valid_to') = (to_jsonb(OLD) - 'valid_to') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION '% on %.% is not allowed: only valid_to may be set, once', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END $$;

-- Turkish IBAN check on the normalised form (no spaces, upper case, 26
-- characters): 'TR' + 24 digits and ISO 13616 mod-97 = 1, computed on
-- substr(iban, 5) || '2927' || substr(iban, 3, 2) (T=29, R=27) in 9-digit
-- pieces. internal/platform/iban implements the same algorithm; both are
-- tested with the vectors of internal/platform/testutil/fake (T-42: the
-- reserved 10th digit is not checked until it is confirmed).
CREATE FUNCTION public.koro_iban_tr_valid(iban text) RETURNS boolean
  LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE STRICT AS $$
DECLARE
  digits text;
  rem    bigint := 0;
  pos    integer := 1;
BEGIN
  IF iban !~ '^TR[0-9]{24}$' THEN
    RETURN false;
  END IF;
  digits := substr(iban, 5) || '2927' || substr(iban, 3, 2);
  WHILE pos <= length(digits) LOOP
    rem := (rem::text || substr(digits, pos, 9))::bigint % 97;
    pos := pos + 9;
  END LOOP;
  RETURN rem = 1;
END $$;
COMMIT;
