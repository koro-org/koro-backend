-- 000015_create_iys_submissions.down.sql
-- DATA LOSS: İYS submission state (pending retries are forgotten).
BEGIN;
DROP TABLE IF EXISTS iys_submissions;
COMMIT;
