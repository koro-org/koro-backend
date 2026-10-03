-- 000019_create_sessions.down.sql
-- DATA LOSS: every session (all users are signed out).
BEGIN;
DROP TABLE IF EXISTS sessions;
COMMIT;
