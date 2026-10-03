-- 000020_create_email_tokens.down.sql
-- DATA LOSS: open e-mail verification and password reset tokens.
BEGIN;
DROP TABLE IF EXISTS email_tokens;
COMMIT;
