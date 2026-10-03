-- 000008_create_function_audit_purge_until.up.sql (02 §2.1 "İmha").
-- audit_purge_until(p_day) deletes the chained audit_events rows covered by
-- the daily root of p_day (chain_seq <= last_chain_seq); verification then
-- continues from that root. It refuses a day inside the retention window
-- (retention_policies 'audit_events', 02 §24.2; the seed value of 2 years
-- applies until that table exists) and a day without a root. SECURITY
-- DEFINER (owner app_owner) so that app_rw needs no DELETE grant; the
-- koro.audit_purge setting opens audit_events_guard for this transaction only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE FUNCTION public.audit_purge_until(p_day date) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_retention interval;
  v_last_seq  bigint;
  v_deleted   bigint;
BEGIN
  IF p_day IS NULL THEN
    RAISE EXCEPTION 'audit_purge_until: day is required' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF to_regclass('public.retention_policies') IS NOT NULL THEN
    EXECUTE 'SELECT retention FROM public.retention_policies WHERE code = $1'
      INTO v_retention USING 'audit_events';
  END IF;
  v_retention := coalesce(v_retention, interval '2 years');
  IF p_day >= ((now() AT TIME ZONE 'Europe/Istanbul')::date - v_retention)::date THEN
    RAISE EXCEPTION 'audit_purge_until: % is inside the retention window (%)', p_day, v_retention
      USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT last_chain_seq INTO v_last_seq FROM public.audit_daily_roots WHERE day = p_day;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'audit_purge_until: no daily root for %', p_day
      USING ERRCODE = 'invalid_parameter_value';
  END IF;
  PERFORM set_config('koro.audit_purge', 'on', true);
  DELETE FROM public.audit_events WHERE chain_seq <= v_last_seq;
  GET DIAGNOSTICS v_deleted = ROW_COUNT;
  PERFORM set_config('koro.audit_purge', 'off', true);
  RETURN v_deleted;
END $$;
REVOKE ALL ON FUNCTION public.audit_purge_until(date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.audit_purge_until(date) TO app_rw;
COMMIT;
