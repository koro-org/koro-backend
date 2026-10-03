-- 000045_create_function_count_segment.up.sql (02 §9.1, 01 §1.4). The only
-- way outreach sizes an audience: the number of eligible students whose
-- audience_keys match, OR inside a dimension and AND across dimensions
-- (uni, dept, year, club; 'all' is a dimension of its own). p_dim_count is
-- the number of distinct dimensions of the targeting. The k >= 10 threshold
-- lives here as well as in the service: a result below 10 is NULL, never the
-- real number. SECURITY INVOKER on purpose: the caller needs SELECT on
-- student_profiles (app_rw has it, app_ro does not), so the function never
-- widens what a role can read. Not callable by PUBLIC.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE FUNCTION public.count_segment(p_keys text[], p_dim_count smallint) RETURNS integer
  LANGUAGE sql STABLE AS $$
  SELECT CASE WHEN c < 10 THEN NULL ELSE c END FROM (
    SELECT count(*)::int AS c FROM public.student_profiles sp
     WHERE sp.audience_eligible
       AND sp.audience_keys && p_keys
       AND (SELECT count(DISTINCT split_part(k, ':', 1)) FROM unnest(sp.audience_keys) k WHERE k = ANY (p_keys))
           = p_dim_count
  ) s $$;
REVOKE ALL ON FUNCTION public.count_segment(text[], smallint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.count_segment(text[], smallint) TO app_rw, app_ro;
COMMIT;
