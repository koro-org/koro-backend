-- 000026_create_email_suppressions.down.sql
-- DATA LOSS: the suppression list (bounced and complaining addresses get mail again).
BEGIN;
DROP TABLE IF EXISTS email_suppressions;
COMMIT;
