-- 000086_create_talent_score_runs.down.sql
-- DATA LOSS: the history of the scoring runs.
BEGIN;
DROP TABLE IF EXISTS talent.score_runs;
COMMIT;
