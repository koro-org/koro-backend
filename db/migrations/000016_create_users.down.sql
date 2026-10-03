-- 000016_create_users.down.sql
-- DATA LOSS: every account.
BEGIN;
DROP TABLE IF EXISTS users;
COMMIT;
