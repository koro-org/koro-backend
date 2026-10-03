-- 000001_init_extensions.down.sql. The schemas are dropped without CASCADE:
-- this fails while a later migration still has objects in them.
BEGIN;
REVOKE SELECT ON public.schema_migrations FROM app_rw;
REVOKE USAGE ON SCHEMA analytics FROM app_ro;
REVOKE USAGE ON SCHEMA talent FROM app_talent;
DROP SCHEMA IF EXISTS analytics;
DROP SCHEMA IF EXISTS talent;
DROP TEXT SEARCH CONFIGURATION IF EXISTS public.koro_tr;
DROP COLLATION IF EXISTS public.tr_ci;
DROP COLLATION IF EXISTS public.tr;
DROP EXTENSION IF EXISTS btree_gist;
DROP EXTENSION IF EXISTS unaccent;
DROP EXTENSION IF EXISTS pg_trgm;
COMMIT;
