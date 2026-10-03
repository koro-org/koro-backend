-- 000001_init_extensions.up.sql (02 §0.3). Extensions, collations, the
-- Turkish FTS configuration and the talent/analytics schemas. Roles are
-- created by deploy/db/bootstrap.sql (H-3), never by a migration.
BEGIN;
SET LOCAL lock_timeout = '5s';

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE EXTENSION IF NOT EXISTS btree_gist; -- commerce_policies exclusion constraint
-- No pgcrypto: sha256() and uuidv7() are in the PostgreSQL 18 core.

-- tr: written explicitly on human name/title columns (COLLATE "tr").
-- tr_ci: nondeterministic; never used on a column or an index in v1, only in
-- ad hoc queries (WHERE a = b COLLATE tr_ci).
CREATE COLLATION IF NOT EXISTS public.tr (provider = icu, locale = 'tr-TR');
CREATE COLLATION IF NOT EXISTS public.tr_ci (provider = icu, locale = 'tr-TR-u-ks-level2', deterministic = false);

-- Turkish stemming is kept; accent folding is done on the trigram side
-- (koro_norm), because unaccent before the stemmer breaks Turkish suffixes.
CREATE TEXT SEARCH CONFIGURATION public.koro_tr (COPY = pg_catalog.turkish);

CREATE SCHEMA IF NOT EXISTS talent AUTHORIZATION app_owner;
CREATE SCHEMA IF NOT EXISTS analytics AUTHORIZATION app_owner;
-- app_talent is the only role that may enter the talent schema (app_rw
-- reaches it only through SET LOCAL ROLE app_talent); app_ro reads analytics.
GRANT USAGE ON SCHEMA talent TO app_talent;
GRANT USAGE ON SCHEMA analytics TO app_ro;

-- /readyz compares the schema version with the binary's newest migration
-- (golang-migrate creates this table before the first migration runs).
GRANT SELECT ON public.schema_migrations TO app_rw;
COMMIT;
