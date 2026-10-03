-- 000099_create_offerings.down.sql
-- DATA LOSS: every offering.
BEGIN;
DROP TABLE IF EXISTS offerings;
DROP FUNCTION IF EXISTS public.offerings_forbid_nondraft_delete();
COMMIT;
