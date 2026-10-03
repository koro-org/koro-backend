-- 000085_create_talent_scores.down.sql
-- DATA LOSS: every talent score.
BEGIN;
DROP TABLE IF EXISTS talent.scores;
COMMIT;
