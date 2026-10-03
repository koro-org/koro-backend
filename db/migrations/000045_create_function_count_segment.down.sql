-- 000045_create_function_count_segment.down.sql
BEGIN;
DROP FUNCTION IF EXISTS public.count_segment(text[], smallint);
COMMIT;
