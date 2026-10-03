-- 000043_create_skills.down.sql
-- DATA LOSS: the skill catalogue (reloaded by `koro seed reference`).
BEGIN;
DROP TABLE IF EXISTS skills;
COMMIT;
