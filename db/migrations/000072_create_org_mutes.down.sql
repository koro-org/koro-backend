-- 000072_create_org_mutes.down.sql
-- DATA LOSS: every mute.
BEGIN;
DROP TABLE IF EXISTS org_mutes;
COMMIT;
