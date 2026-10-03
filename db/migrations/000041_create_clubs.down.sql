-- 000041_create_clubs.down.sql
-- DATA LOSS: club profile data, including the sensitivity flags.
BEGIN;
DROP TABLE IF EXISTS clubs;
COMMIT;
