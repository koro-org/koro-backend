-- 000031_seed_platform_org.down.sql
-- DATA LOSS: the platform org row. It fails while a membership or content
-- still refers to it (their tables are dropped first in a full down).
BEGIN;
DELETE FROM organizations WHERE kind = 'platform' AND slug = 'koro';
COMMIT;
