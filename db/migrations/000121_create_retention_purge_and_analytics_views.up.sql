-- 000121_create_retention_purge_and_analytics_views.up.sql (02 §23 000121;
-- BE-049, kvkk-2; 06 H-34, H-41, H-48; T-120, T-121, T-153, T-192).
--
-- 1. Retention purge of the append-only tables. consents,
--    moderation_actions, destruction_logs, talent.point_events and
--    talent.config_versions are guarded by koro_forbid_mutation (or its
--    moderation variant) and app_rw / app_talent hold no DELETE: nobody,
--    the owner included, could destroy a row at the end of its retention
--    period. Following audit_purge_until (000008), every table gets a
--    SECURITY DEFINER purge function (owner app_owner) that:
--      * reads the period of its retention_policies row and never deletes
--        inside it: the cut-off is least(p_cutoff, now() - retention), so
--        a caller's clock skew can only delete less;
--      * deletes only when the policy is enabled AND legally approved
--        (T-35): until the lawyer signs, the call raises 55000 and only the
--        dry run (p_dry_run = true: count, no deletion) works;
--      * deletes in batches of at most p_limit ≤ 1000 rows (01 §7), oldest
--        first, so a run that is interrupted simply continues next time;
--      * opens the guard for its own table only, through the
--        transaction-local setting koro.retention_purge = '<schema>.<table>',
--        and closes it again before returning. The setting alone grants
--        nothing: the roles of the application still hold no DELETE.
--    Who writes destruction_logs: the caller (kvkk.retention_sweep writes
--    one row per policy and run, 01 §7).
-- 2. retention_policies: role_history is anchored on the end of the
--    assignment, which platform_role_assignments records as revoked_at or
--    rejected_at (no ended_at; T-121) → new anchor 'revoked_or_rejected';
--    dsr_export_file is 7 days (06 H-48, T-83/T-192). Approved rows are
--    never overwritten.
-- 3. analytics.* views for Metabase (01 §1.4): aggregates only, no person's
--    ID, name or timestamp finer than a month; a group of fewer than 10
--    people is left out (k ≥ 10, HAVING count(*) >= 10); sensitive clubs and
--    sensitive-category events are left out (special category data, KVKK
--    md. 6); talent bands exclude neutralised users. Only app_ro reads them
--    (app_rw has no USAGE on the schema, 000001); the views run with the
--    owner's privileges, so app_ro still reads no table.
BEGIN;
SET LOCAL lock_timeout = '5s';

-- 1a. The append-only guards let a purge function through, for its own
-- table and for DELETE only (UPDATE and TRUNCATE stay forbidden).
CREATE OR REPLACE FUNCTION public.koro_forbid_mutation() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' AND current_setting('koro.retention_purge', true) = TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION '% on %.% is not allowed: the table is append-only', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END $$;

CREATE OR REPLACE FUNCTION public.moderation_actions_forbid_mutation() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' AND current_setting('koro.retention_purge', true) = TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME THEN
    RETURN OLD;
  END IF;
  IF TG_OP = 'UPDATE'
     AND OLD.report_id IS NOT NULL AND NEW.report_id IS NULL
     AND (to_jsonb(NEW) - 'report_id') = (to_jsonb(OLD) - 'report_id') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION '% on %.% is not allowed: the table is append-only (only report_id may be cleared)', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END $$;

-- 1b. koro_retention_cutoff returns the cut-off a purge may use: the
-- earlier of the caller's and the policy's (now() - retention). A real
-- purge (not a dry run) needs the policy enabled and legally approved.
CREATE FUNCTION public.koro_retention_cutoff(p_code text, p_cutoff timestamptz, p_limit integer, p_dry_run boolean)
  RETURNS timestamptz
  LANGUAGE plpgsql STABLE
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_retention interval;
  v_enabled   boolean;
  v_approved  boolean;
BEGIN
  IF p_cutoff IS NULL OR p_dry_run IS NULL THEN
    RAISE EXCEPTION 'retention purge %: cut-off and dry-run flag are required', p_code USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 THEN
    RAISE EXCEPTION 'retention purge %: batch size must be 1..1000', p_code USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT retention, enabled, legal_approved INTO v_retention, v_enabled, v_approved
    FROM public.retention_policies WHERE code = p_code;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'retention purge: no retention policy %', p_code USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF NOT p_dry_run AND NOT (v_enabled AND v_approved) THEN
    RAISE EXCEPTION 'retention purge: policy % is not enabled and legally approved (dry run only)', p_code
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  RETURN least(p_cutoff, now() - v_retention);
END $$;
REVOKE ALL ON FUNCTION public.koro_retention_cutoff(text, timestamptz, integer, boolean) FROM PUBLIC;

-- 1c. consents + iys_submissions (consent_proof: 3 years after the
-- withdrawal or the account erasure). A consent chain (user, purpose,
-- scope: the key of consent_current) goes when its latest row is a
-- withdrawal older than the cut-off, or when the user was erased before
-- the cut-off. Rows go oldest first, so a chain cut by the batch size keeps
-- its newest rows: its current state (withdrawn) never changes.
CREATE FUNCTION public.consents_purgeable(p_cutoff timestamptz)
  RETURNS TABLE (id uuid, occurred_at timestamptz)
  LANGUAGE sql STABLE
  SET search_path = pg_catalog, public
  AS $$
  WITH latest AS (
    SELECT DISTINCT ON (c.user_id, c.purpose_code, c.scope_id) c.user_id, c.purpose_code, c.scope_id, c.action, c.occurred_at
      FROM public.consents c
     ORDER BY c.user_id, c.purpose_code, c.scope_id, c.occurred_at DESC, c.id DESC
  ), doomed AS (
    SELECT l.user_id, l.purpose_code, l.scope_id
      FROM latest l JOIN public.users u ON u.id = l.user_id
     WHERE (l.action = 'withdrawn' AND l.occurred_at < p_cutoff)
        OR (u.status = 'erased' AND u.erased_at < p_cutoff)
  )
  SELECT c.id, c.occurred_at
    FROM public.consents c
    JOIN doomed d ON d.user_id = c.user_id AND d.purpose_code = c.purpose_code AND d.scope_id IS NOT DISTINCT FROM c.scope_id
$$;
REVOKE ALL ON FUNCTION public.consents_purgeable(timestamptz) FROM PUBLIC;

CREATE FUNCTION public.consents_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('consent_proof', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
BEGIN
  IF p_dry_run THEN
    SELECT count(*) INTO v_n FROM public.consents_purgeable(v_cutoff);
    RETURN v_n;
  END IF;
  v_ids := ARRAY(SELECT p.id FROM public.consents_purgeable(v_cutoff) p ORDER BY p.occurred_at, p.id LIMIT p_limit);
  IF cardinality(v_ids) = 0 THEN
    RETURN 0;
  END IF;
  DELETE FROM public.iys_submissions WHERE consent_id = ANY (v_ids);
  PERFORM set_config('koro.retention_purge', 'public.consents', true);
  DELETE FROM public.consents WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.consents_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.consents_purge(timestamptz, integer, boolean) TO app_rw;

-- 1d. moderation_actions (moderation_action: 2 years after created_at).
CREATE FUNCTION public.moderation_actions_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('moderation_action', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
BEGIN
  IF p_dry_run THEN
    SELECT count(*) INTO v_n FROM public.moderation_actions WHERE created_at < v_cutoff;
    RETURN v_n;
  END IF;
  v_ids := ARRAY(SELECT m.id FROM public.moderation_actions m WHERE m.created_at < v_cutoff ORDER BY m.created_at, m.id LIMIT p_limit);
  IF cardinality(v_ids) = 0 THEN
    RETURN 0;
  END IF;
  PERFORM set_config('koro.retention_purge', 'public.moderation_actions', true);
  DELETE FROM public.moderation_actions WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.moderation_actions_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.moderation_actions_purge(timestamptz, integer, boolean) TO app_rw;

-- 1e. destruction_logs (destruction_log: 3 years after created_at).
CREATE FUNCTION public.destruction_logs_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('destruction_log', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
BEGIN
  IF p_dry_run THEN
    SELECT count(*) INTO v_n FROM public.destruction_logs WHERE created_at < v_cutoff;
    RETURN v_n;
  END IF;
  v_ids := ARRAY(SELECT l.id FROM public.destruction_logs l WHERE l.created_at < v_cutoff ORDER BY l.created_at, l.id LIMIT p_limit);
  IF cardinality(v_ids) = 0 THEN
    RETURN 0;
  END IF;
  PERFORM set_config('koro.retention_purge', 'public.destruction_logs', true);
  DELETE FROM public.destruction_logs WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.destruction_logs_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.destruction_logs_purge(timestamptz, integer, boolean) TO app_rw;

-- 1f. talent.point_events (talent_ledger: 24 months after created_at). A
-- reversal points at its original (point_events_reversal_of_fkey): old
-- reversals go first; an original goes once no reversal points at it (a
-- younger reversal keeps it until the reversal itself is old enough).
CREATE FUNCTION talent.point_events_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('talent_ledger', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
BEGIN
  IF p_dry_run THEN
    SELECT count(*) INTO v_n FROM talent.point_events p
     WHERE p.created_at < v_cutoff
       AND NOT EXISTS (SELECT 1 FROM talent.point_events r WHERE r.reversal_of = p.id AND r.created_at >= v_cutoff);
    RETURN v_n;
  END IF;
  v_ids := ARRAY(SELECT p.id FROM talent.point_events p
                  WHERE p.created_at < v_cutoff AND p.reversal_of IS NOT NULL
                  ORDER BY p.created_at, p.id LIMIT p_limit);
  IF cardinality(v_ids) = 0 THEN
    v_ids := ARRAY(SELECT p.id FROM talent.point_events p
                    WHERE p.created_at < v_cutoff AND p.reversal_of IS NULL
                      AND NOT EXISTS (SELECT 1 FROM talent.point_events r WHERE r.reversal_of = p.id)
                    ORDER BY p.created_at, p.id LIMIT p_limit);
  END IF;
  IF cardinality(v_ids) = 0 THEN
    RETURN 0;
  END IF;
  PERFORM set_config('koro.retention_purge', 'talent.point_events', true);
  DELETE FROM talent.point_events WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION talent.point_events_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION talent.point_events_purge(timestamptz, integer, boolean) TO app_talent;

-- 1g. talent.point_events of an erased account (KVKK md. 7; T-153): the
-- whole ledger of the user goes, but only once an erasure request of that
-- user is completed (the DPO answered it, 02 §16.1). One statement: the
-- reversal foreign keys are checked at its end.
CREATE FUNCTION talent.point_events_erase_user(p_user_id uuid) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_n bigint;
BEGIN
  IF p_user_id IS NULL THEN
    RAISE EXCEPTION 'point_events_erase_user: user is required' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM public.data_subject_requests
                  WHERE user_id = p_user_id AND type = 'erasure' AND status = 'completed') THEN
    RAISE EXCEPTION 'point_events_erase_user: no completed erasure request for the user'
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  PERFORM set_config('koro.retention_purge', 'talent.point_events', true);
  DELETE FROM talent.point_events WHERE user_id = p_user_id;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION talent.point_events_erase_user(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION talent.point_events_erase_user(uuid) TO app_talent;

-- 1h. talent.config_versions. No policy of its own in 02 §24.2: a version
-- explains ledger rows, so it follows talent_ledger. A version goes when a
-- later version took effect before the cut-off (it ended then) and no
-- ledger row, score or score run refers to it any more.
CREATE FUNCTION talent.config_versions_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('talent_ledger', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
BEGIN
  v_ids := ARRAY(SELECT v.id FROM talent.config_versions v
                  WHERE EXISTS (SELECT 1 FROM talent.config_versions s
                                 WHERE s.effective_from > v.effective_from AND s.effective_from < v_cutoff
                                   AND s.effective_from <= now())
                    AND NOT EXISTS (SELECT 1 FROM talent.point_events p WHERE p.config_version_id = v.id)
                    AND NOT EXISTS (SELECT 1 FROM talent.scores c WHERE c.config_version_id = v.id)
                    AND NOT EXISTS (SELECT 1 FROM talent.score_runs r WHERE r.config_version_id = v.id)
                  ORDER BY v.effective_from, v.id
                  LIMIT CASE WHEN p_dry_run THEN NULL ELSE p_limit END);
  IF p_dry_run OR cardinality(v_ids) = 0 THEN
    RETURN cardinality(v_ids);
  END IF;
  PERFORM set_config('koro.retention_purge', 'talent.config_versions', true);
  DELETE FROM talent.config_versions WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION talent.config_versions_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION talent.config_versions_purge(timestamptz, integer, boolean) TO app_talent;

-- 1i. privileged_account_actions (BE-016, 06 H-47): the four-eyes requests
-- against privileged accounts are proof of the authority review, like
-- role_history; policy privileged_action_history (below). app_rw holds no
-- DELETE on the table (BE-016), so its sweeper deletes through this
-- function. A request ends when it is approved (approved_at) or lapses
-- (expires_at): that is its decided_at anchor.
CREATE FUNCTION public.privileged_account_actions_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('privileged_action_history', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
BEGIN
  v_ids := ARRAY(SELECT a.id FROM public.privileged_account_actions a
                  WHERE CASE WHEN a.status = 'approved' THEN a.approved_at ELSE a.expires_at END < v_cutoff
                  ORDER BY a.requested_at, a.id
                  LIMIT CASE WHEN p_dry_run THEN NULL ELSE p_limit END);
  IF p_dry_run OR cardinality(v_ids) = 0 THEN
    RETURN cardinality(v_ids);
  END IF;
  DELETE FROM public.privileged_account_actions WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.privileged_account_actions_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.privileged_account_actions_purge(timestamptz, integer, boolean) TO app_rw;

-- 2. Retention policy alignment (T-121, 06 H-48) and the policy of the
-- privileged account actions (BE-016; with role_history, unapproved like
-- every seeded row, T-35).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('privileged_action_history', 'identity', 'privileged_account_actions', '3 years', 'decided_at', 'hard_delete',
   'Yetki gözden geçirme ispatı', true, 'Onaylanmış veya süresi dolmuş ayrıcalıklı hesap işlemi talepleri (4 göz) silinir.')
ON CONFLICT (code) DO NOTHING;
ALTER TABLE public.retention_policies DROP CONSTRAINT retention_policies_anchor_chk;
ALTER TABLE public.retention_policies ADD CONSTRAINT retention_policies_anchor_chk CHECK (anchor IN ('created_at','ended_at','closed_at',
  'expires_at','event_end','decided_at','withdrawn_or_erased','last_seen_at','published_at','revoked_or_rejected'));
UPDATE public.retention_policies SET anchor = 'revoked_or_rejected'
 WHERE code = 'role_history' AND anchor = 'ended_at' AND NOT legal_approved;
UPDATE public.retention_policies SET retention = interval '7 days'
 WHERE code = 'dsr_export_file' AND retention > interval '7 days' AND NOT legal_approved;

-- 3. analytics.* (k ≥ 10).
-- New accounts per month and sign-up persona (tombstones included: the
-- account existed).
CREATE VIEW analytics.signups_monthly WITH (security_barrier = true) AS
  SELECT date_trunc('month', u.created_at AT TIME ZONE 'Europe/Istanbul')::date AS month,
         u.signup_persona AS persona,
         count(*)::integer AS accounts
    FROM public.users u
   GROUP BY 1, 2
  HAVING count(*) >= 10;

-- Active students per university and degree level.
CREATE VIEW analytics.students_by_university WITH (security_barrier = true) AS
  SELECT un.id AS university_id, un.name AS university, sp.degree_level,
         count(*)::integer AS students
    FROM public.student_profiles sp
    JOIN public.users u ON u.id = sp.user_id
    JOIN public.universities un ON un.id = sp.university_id
   WHERE sp.status = 'active' AND u.status = 'active'
   GROUP BY un.id, un.name, sp.degree_level
  HAVING count(*) >= 10;

-- Event participation per month, organiser kind and event category, without
-- sensitive clubs and sensitive-category events. checked_in is NULL when
-- fewer than 10 people checked in.
CREATE VIEW analytics.event_participation_monthly WITH (security_barrier = true) AS
  SELECT date_trunc('month', e.starts_at AT TIME ZONE 'Europe/Istanbul')::date AS month,
         o.kind AS org_kind, e.category,
         count(*)::integer AS registrations,
         CASE WHEN count(*) FILTER (WHERE r.status = 'checked_in') >= 10
              THEN (count(*) FILTER (WHERE r.status = 'checked_in'))::integer END AS checked_in
    FROM public.event_registrations r
    JOIN public.events e ON e.id = r.event_id
    JOIN public.organizations o ON o.id = e.org_id
    LEFT JOIN public.clubs c ON c.org_id = e.org_id
   WHERE r.status IN ('confirmed','checked_in','no_show')
     AND NOT e.is_sensitive_category
     AND NOT coalesce(c.is_sensitive, false)
   GROUP BY 1, 2, 3
  HAVING count(*) >= 10;

-- Talent band distribution per university and department, the bias audit
-- of R-003 (band only: no score, no percentile); neutralised users are left
-- out. There is deliberately no coarser talent view: a university total
-- next to its department cells would reveal suppressed cells by difference.
CREATE VIEW analytics.talent_distribution WITH (security_barrier = true) AS
  SELECT un.id AS university_id, un.name AS university, d.id AS department_id, d.name AS department,
         s.band, count(*)::integer AS students
    FROM talent.scores s
    JOIN public.student_profiles sp ON sp.user_id = s.user_id
    JOIN public.universities un ON un.id = sp.university_id
    LEFT JOIN public.departments d ON d.id = sp.department_id
   WHERE NOT s.neutralized
   GROUP BY un.id, un.name, d.id, d.name, s.band
  HAVING count(*) >= 10;

REVOKE ALL ON analytics.signups_monthly, analytics.students_by_university,
  analytics.event_participation_monthly, analytics.talent_distribution FROM PUBLIC;
GRANT SELECT ON analytics.signups_monthly, analytics.students_by_university,
  analytics.event_participation_monthly, analytics.talent_distribution TO app_ro;
COMMIT;
