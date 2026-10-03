-- 000079_create_talent_config_versions.down.sql
-- DATA LOSS: every talent config version. (The REVOKEs of the up file only
-- restated the default: there is nothing to give back.)
BEGIN;
DROP TABLE IF EXISTS talent.config_versions;
COMMIT;
