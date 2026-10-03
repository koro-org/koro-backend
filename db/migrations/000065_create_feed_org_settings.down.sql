-- 000065_create_feed_org_settings.down.sql
-- DATA LOSS: the feed trust level of every org.
BEGIN;
DROP TABLE IF EXISTS feed_org_settings;
COMMIT;
