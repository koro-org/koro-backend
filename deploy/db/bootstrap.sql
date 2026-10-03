-- deploy/db/bootstrap.sql — database roles of Koro (06 H-3; 02 §0.3; 05 §15.4).
-- Infrastructure script, NOT a migration: run once per cluster/database by a
-- superuser, before the first `koro migrate up`. Migrations only GRANT/REVOKE.
--
--   local:  deploy/compose.dev.yaml mounts it into docker-entrypoint-initdb.d
--           (with deploy/db/dev-passwords.sql right after it)
--   tests:  internal/platform/testutil runs the same two files
--   prod:   OPS runs it with psql against the koro database, then sets the
--           passwords from secrets (ALTER ROLE ... PASSWORD); it never runs
--           dev-passwords.sql
--
-- It sets no password and is idempotent (re-running only re-applies the
-- memberships and the database owner). The database itself is created with
-- the ICU tr-TR locale (02 §0.3):
--   initdb --encoding=UTF8 --locale-provider=icu --icu-locale=tr-TR --locale=C.UTF-8
--
-- Roles:
--   app_owner      owns every table; only `koro migrate` (MIGRATE_DATABASE_URL) logs in as it
--   app_rw         api + worker (DATABASE_URL); no privilege on the talent schema
--   app_ro         Metabase; analytics views only
--   audit_writer   INSERT on audit_events; app_rw is a member WITH INHERIT TRUE
--   audit_chainer  UPDATE of the audit chain columns; app_rw is a member
--                  WITH INHERIT FALSE, SET TRUE (needs SET LOCAL ROLE audit_chainer)
--   app_talent     the only role inside the talent schema; app_rw is a member
--                  WITH INHERIT FALSE, SET TRUE (needs SET LOCAL ROLE app_talent)

DO $$
DECLARE
  r record;
BEGIN
  FOR r IN
    SELECT * FROM (VALUES
      ('app_owner',     true),
      ('app_rw',        true),
      ('app_ro',        true),
      ('audit_writer',  false),
      ('audit_chainer', false),
      ('app_talent',    false)
    ) AS t(name, login)
  LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = r.name) THEN
      EXECUTE format('CREATE ROLE %I %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS',
                     r.name, CASE WHEN r.login THEN 'LOGIN' ELSE 'NOLOGIN' END);
    END IF;
  END LOOP;
END $$;

-- PostgreSQL 16+ membership options (T-45).
GRANT audit_writer  TO app_rw WITH INHERIT TRUE,  SET FALSE;
GRANT audit_chainer TO app_rw WITH INHERIT FALSE, SET TRUE;
GRANT app_talent    TO app_rw WITH INHERIT FALSE, SET TRUE;

-- app_owner owns the current database: it may create extensions (pg_trgm,
-- unaccent and btree_gist are trusted), schemas and collations without
-- being a superuser; the public schema belongs to pg_database_owner.
DO $$
BEGIN
  EXECUTE format('ALTER DATABASE %I OWNER TO app_owner', current_database());
END $$;

-- Only the application roles may connect (PUBLIC gets CONNECT by default).
DO $$
BEGIN
  EXECUTE format('REVOKE CONNECT ON DATABASE %I FROM PUBLIC', current_database());
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO app_owner, app_rw, app_ro', current_database());
END $$;
