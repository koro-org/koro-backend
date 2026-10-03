-- deploy/db/dev-passwords.sql — LOCAL DEVELOPMENT AND TESTS ONLY.
-- Runs right after deploy/db/bootstrap.sql in deploy/compose.dev.yaml and in
-- internal/platform/testutil. Production passwords come from secrets (OPS);
-- this file is never run outside dev/test. Matches .env.example.
ALTER ROLE app_owner PASSWORD 'dev-only-change-me';
ALTER ROLE app_rw    PASSWORD 'dev-only-change-me';
ALTER ROLE app_ro    PASSWORD 'dev-only-change-me';
