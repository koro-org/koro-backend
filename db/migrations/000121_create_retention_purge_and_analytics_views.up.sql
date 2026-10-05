-- 000121_create_retention_purge_and_analytics_views.up.sql (02 §23 000121;
-- BE-049, kvkk-2; 06 H-34, H-41, H-48; T-120, T-121, T-153, T-192;
-- extended in place by BE-057, kvkk-3: 06 H-57, T-149, T-217, T-224).
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
--    BE-057 adds the same kind of function for the rows app_rw cannot
--    delete at the end of their retention: entitlements with their
--    append-only usages (1j), the signed audit roots (1k) and the financial
--    records of 000094–000120 (1l, 06 H-57), plus a purge branch in the
--    payout item guard of 000115.
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

-- 1j. entitlements + entitlement_usages (entitlement_other: 2 years after
-- the right ended; BE-057, T-149/T-217). An admin or promo grant ends at
-- revoked_at, or at valid_to. Its usages are append-only, so the grant and
-- its usages go together here; a usage that still proves a targeted post,
-- a dispatch or a contact request (their FKs) keeps the whole grant until
-- that record is gone. Order rights (source_type order_line) follow the
-- order, not this policy. A batch is bounded by rows (grant + its usages
-- ≤ p_limit; the first grant always goes, so a big one never blocks).
CREATE FUNCTION public.entitlements_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('entitlement_other', p_cutoff, p_limit, p_dry_run);
  v_ids    uuid[];
  v_n      bigint;
  v_u      bigint;
BEGIN
  v_ids := ARRAY(
    SELECT c.id FROM (
      SELECT e.id, sum(1 + (SELECT count(*) FROM public.entitlement_usages u WHERE u.entitlement_id = e.id))
                     OVER (ORDER BY least(e.revoked_at, e.valid_to), e.id) AS running,
             row_number() OVER (ORDER BY least(e.revoked_at, e.valid_to), e.id) AS rn
        FROM public.entitlements e
       WHERE e.source_type IN ('admin_grant','promo')
         AND least(e.revoked_at, e.valid_to) < v_cutoff
         AND NOT EXISTS (SELECT 1 FROM public.entitlement_usages u
                          WHERE u.entitlement_id = e.id
                            AND (EXISTS (SELECT 1 FROM public.posts p WHERE p.entitlement_usage_id = u.id)
                              OR EXISTS (SELECT 1 FROM public.outreach_dispatches d WHERE d.entitlement_usage_id = u.id)
                              OR EXISTS (SELECT 1 FROM public.contact_requests r WHERE r.entitlement_usage_id = u.id)))) c
     WHERE p_dry_run OR c.rn = 1 OR c.running <= p_limit
     ORDER BY c.rn);
  IF p_dry_run OR cardinality(v_ids) = 0 THEN
    RETURN cardinality(v_ids);
  END IF;
  PERFORM set_config('koro.retention_purge', 'public.entitlement_usages', true);
  DELETE FROM public.entitlement_usages WHERE entitlement_id = ANY (v_ids);
  GET DIAGNOSTICS v_u = ROW_COUNT;
  PERFORM set_config('koro.retention_purge', '', true);
  DELETE FROM public.entitlements WHERE id = ANY (v_ids);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n + v_u;
END $$;
REVOKE ALL ON FUNCTION public.entitlements_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.entitlements_purge(timestamptz, integer, boolean) TO app_rw;

-- 1k. audit_daily_roots (audit_roots: 10 years; BE-057). app_rw holds
-- SELECT/INSERT only. Verification of the chain starts at a root (the
-- newest one whose rows audit_purge_until removed), so a root goes only
-- when it is older than the cut-off and a later root exists whose chained
-- rows are all gone: that later root is where verification starts.
CREATE FUNCTION public.audit_daily_roots_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff timestamptz := public.koro_retention_cutoff('audit_roots', p_cutoff, p_limit, p_dry_run);
  v_days   date[];
  v_n      bigint;
BEGIN
  v_days := ARRAY(SELECT r.day FROM public.audit_daily_roots r
                   WHERE r.created_at < v_cutoff
                     AND EXISTS (SELECT 1 FROM public.audit_daily_roots s
                                  WHERE s.day > r.day
                                    AND NOT EXISTS (SELECT 1 FROM public.audit_events e WHERE e.chain_seq <= s.last_chain_seq))
                   ORDER BY r.day
                   LIMIT CASE WHEN p_dry_run THEN NULL ELSE p_limit END);
  IF p_dry_run OR cardinality(v_days) = 0 THEN
    RETURN cardinality(v_days);
  END IF;
  DELETE FROM public.audit_daily_roots WHERE day = ANY (v_days);
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.audit_daily_roots_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.audit_daily_roots_purge(timestamptz, integer, boolean) TO app_rw;

-- 1l. Financial records (financial_records: 10 years; 06 H-57, T-224;
-- BE-057). TTK md. 82 and VUK md. 253 count the 10 years from the end of
-- the calendar year of the record, so the cut-off is rounded down to the
-- start of its year (Europe/Istanbul): a record of 2026 goes on 1 January
-- 2037 at the earliest. app_rw holds no DELETE on these tables and the
-- ledger is append-only; this SECURITY DEFINER function is the only way.
--
-- What goes, and only as a whole:
--   * an order aggregate: the order, its lines, payments, payment splits,
--     refunds, the bank transfers matched to it, its invoices (with their
--     lines and credit notes) and every ledger transaction about it (ref
--     order / payment / refund, or an entry on one of its lines), together
--     with the payouts that paid its lines (items, payout invoices, payout
--     ledger transactions) and the other orders those payouts paid. Every
--     row of that closure is older than the cut-off (created_at and the
--     last update), and the closure is balanced: on the per-payee accounts
--     (seller_payable) each line nets to zero and no entry lacks its line,
--     so no payee balance changes (the ledger's integrity check stays true).
--     A young refund, an open payable, a payout of a young order or a
--     transaction that also touches a young line keeps the whole closure.
--   * then, unreferenced master and loose rows older than the cut-off:
--     failed / cancelled payouts without items, unmatched transfers,
--     invoices of neither an order nor a payout, superseded / rejected /
--     revoked bank accounts no payout or newer account points at, draft or
--     closed payees nothing refers to, legal parties (with their documents)
--     nothing refers to. An approved account and an active or suspended
--     payee are in use and stay, however old.
-- Ledger transactions with a manual reference that touch no order line
-- (fee, adjustment) stay: deleting them would move balances (T-new note in
-- BE-057). The files of the deleted rows (receipts, invoices, documents)
-- stay attached to their module; their deletion is BE-057's open point.
-- Batch: p_limit order aggregates (closed over shared payouts) and at most
-- p_limit rows of each loose kind per call; the caller loops until 0.
CREATE FUNCTION public.financial_records_cutoff(p_cutoff timestamptz, p_limit integer, p_dry_run boolean)
  RETURNS timestamptz
  LANGUAGE sql STABLE
  SET search_path = pg_catalog, public
  AS $$
  SELECT date_trunc('year', public.koro_retention_cutoff('financial_records', p_cutoff, p_limit, p_dry_run)
                            AT TIME ZONE 'Europe/Istanbul') AT TIME ZONE 'Europe/Istanbul'
$$;
REVOKE ALL ON FUNCTION public.financial_records_cutoff(timestamptz, integer, boolean) FROM PUBLIC;

-- The payout guard lets the purge through, for DELETE only (an item of a
-- paid payout is otherwise never deleted).
CREATE OR REPLACE FUNCTION public.payout_items_check() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  payout_status text;
  line          record;
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF current_setting('koro.retention_purge', true) = TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME THEN
      RETURN OLD;
    END IF;
    SELECT p.status INTO payout_status FROM payouts p WHERE p.id = OLD.payout_id;
    IF payout_status IS NOT NULL AND payout_status NOT IN ('failed','cancelled') THEN
      RAISE EXCEPTION 'DELETE on %.% is not allowed: payout % is %', TG_TABLE_SCHEMA, TG_TABLE_NAME, OLD.payout_id, payout_status
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
  END IF;
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'UPDATE on %.% is not allowed: an item is replaced by delete + insert', TG_TABLE_SCHEMA, TG_TABLE_NAME
      USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT p.status INTO payout_status FROM payouts p WHERE p.id = NEW.payout_id;
  SELECT l.fulfillment_status, l.payout_id, l.seller_net_amount INTO line FROM order_lines l WHERE l.id = NEW.order_line_id;
  IF payout_status IS DISTINCT FROM 'pending_approval' OR line.fulfillment_status IS DISTINCT FROM 'fulfilled'
     OR line.payout_id IS NOT NULL OR NEW.amount > line.seller_net_amount THEN
    RAISE EXCEPTION 'payout %: line % cannot be added (payout %, line %, paid out %, amount % of %)', NEW.payout_id, NEW.order_line_id,
      payout_status, line.fulfillment_status, line.payout_id IS NOT NULL, NEW.amount, line.seller_net_amount
      USING ERRCODE = 'check_violation', CONSTRAINT = 'payout_items_line_chk';
  END IF;
  RETURN NEW;
END $$;

-- financial_order_ok: the order's own rows are older than the cut-off and
-- its ledger side is closed (every transaction about it is old, its lines
-- net to zero on the per-payee accounts, no per-payee entry without a line,
-- no credit note of another order on its invoices).
CREATE FUNCTION public.financial_order_ok(p_order uuid, p_cutoff timestamptz) RETURNS boolean
  LANGUAGE sql STABLE
  SET search_path = pg_catalog, public
  AS $$
  WITH txn AS (
    SELECT x.id, x.created_at FROM public.ledger_transactions x
     WHERE (x.ref_type = 'order' AND x.ref_id = p_order)
        OR (x.ref_type = 'payment' AND x.ref_id IN (SELECT p.id FROM public.payments p WHERE p.order_id = p_order))
        OR (x.ref_type = 'refund' AND x.ref_id IN (SELECT r.id FROM public.refunds r WHERE r.order_id = p_order))
        OR EXISTS (SELECT 1 FROM public.ledger_entries e JOIN public.order_lines l ON l.id = e.order_line_id
                    WHERE e.transaction_id = x.id AND l.order_id = p_order)
  )
  SELECT NOT EXISTS (SELECT 1 FROM public.order_lines l WHERE l.order_id = p_order AND greatest(l.created_at, l.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.payments p WHERE p.order_id = p_order AND greatest(p.created_at, p.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.payment_splits s WHERE s.order_id = p_order AND greatest(s.created_at, s.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.refunds r WHERE r.order_id = p_order AND greatest(r.created_at, r.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.incoming_bank_transfers t WHERE t.matched_order_id = p_order
                                                                     AND greatest(t.created_at, t.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.invoices i WHERE i.order_id = p_order AND greatest(i.created_at, i.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.invoices i JOIN public.invoices j ON j.refers_to_invoice_id = i.id
                      WHERE i.order_id = p_order AND j.order_id IS DISTINCT FROM p_order)
     AND NOT EXISTS (SELECT 1 FROM txn WHERE txn.created_at >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM txn JOIN public.ledger_entries e ON e.transaction_id = txn.id
                                      JOIN public.ledger_accounts a ON a.code = e.account_code
                      WHERE a.per_payee AND e.order_line_id IS NULL)
     AND NOT EXISTS (SELECT 1 FROM public.order_lines l
                      WHERE l.order_id = p_order
                        AND (SELECT coalesce(sum(e.amount), 0) FROM public.ledger_entries e
                               JOIN public.ledger_accounts a ON a.code = e.account_code
                              WHERE e.order_line_id = l.id AND a.per_payee) <> 0)
$$;
REVOKE ALL ON FUNCTION public.financial_order_ok(uuid, timestamptz) FROM PUBLIC;

-- financial_payout_ok: the payout and its items, invoices and ledger
-- transactions are older than the cut-off and its transactions name the
-- line of every per-payee entry.
CREATE FUNCTION public.financial_payout_ok(p_payout uuid, p_cutoff timestamptz) RETURNS boolean
  LANGUAGE sql STABLE
  SET search_path = pg_catalog, public
  AS $$
  SELECT EXISTS (SELECT 1 FROM public.payouts p WHERE p.id = p_payout AND greatest(p.created_at, p.updated_at) < p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.payout_items i WHERE i.payout_id = p_payout AND i.created_at >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.invoices v WHERE v.payout_id = p_payout AND greatest(v.created_at, v.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.invoices v JOIN public.invoices j ON j.refers_to_invoice_id = v.id
                      WHERE v.payout_id = p_payout AND j.payout_id IS DISTINCT FROM p_payout)
     AND NOT EXISTS (SELECT 1 FROM public.ledger_transactions x
                      WHERE x.ref_type = 'payout' AND x.ref_id = p_payout
                        AND (x.created_at >= p_cutoff
                          OR EXISTS (SELECT 1 FROM public.ledger_entries e JOIN public.ledger_accounts a ON a.code = e.account_code
                                      WHERE e.transaction_id = x.id AND a.per_payee AND e.order_line_id IS NULL)))
$$;
REVOKE ALL ON FUNCTION public.financial_payout_ok(uuid, timestamptz) FROM PUBLIC;

CREATE FUNCTION public.financial_records_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff  timestamptz := public.financial_records_cutoff(p_cutoff, p_limit, p_dry_run);
  v_limit   integer := CASE WHEN p_dry_run THEN NULL ELSE p_limit END;
  v_orders  uuid[];
  v_bad     uuid[];
  v_sel     uuid[];
  v_grow    uuid[];
  v_lines   uuid[];
  v_payouts uuid[];
  v_txns    uuid[];
  v_inv     uuid[];
  v_ids     uuid[];
  v_n       bigint := 0;
  v_k       bigint;
BEGIN
  -- 1. Orders whose own aggregate is closed and old.
  v_orders := ARRAY(SELECT o.id FROM public.orders o
                     WHERE greatest(o.created_at, o.updated_at) < v_cutoff
                       AND public.financial_order_ok(o.id, v_cutoff));
  -- 2. Fixpoint: an order whose line is in a payout that cannot go (young,
  -- or paying a line of an order that cannot go), or whose ledger
  -- transaction or invoice line also touches such an order, cannot go.
  LOOP
    v_bad := ARRAY(
      SELECT DISTINCT l.order_id FROM public.order_lines l
       WHERE l.order_id = ANY (v_orders)
         AND (EXISTS (SELECT 1 FROM public.payout_items i
                       WHERE i.order_line_id = l.id
                         AND (NOT public.financial_payout_ok(i.payout_id, v_cutoff)
                           OR EXISTS (SELECT 1 FROM public.payout_items i2 JOIN public.order_lines l2 ON l2.id = i2.order_line_id
                                       WHERE i2.payout_id = i.payout_id AND l2.order_id <> ALL (v_orders))))
           OR (l.payout_id IS NOT NULL AND NOT public.financial_payout_ok(l.payout_id, v_cutoff))
           OR EXISTS (SELECT 1 FROM public.ledger_entries e
                        JOIN public.ledger_entries e2 ON e2.transaction_id = e.transaction_id
                        JOIN public.order_lines l2 ON l2.id = e2.order_line_id
                       WHERE e.order_line_id = l.id AND l2.order_id <> ALL (v_orders))
           OR EXISTS (SELECT 1 FROM public.invoice_lines il JOIN public.invoices v ON v.id = il.invoice_id
                       WHERE il.order_line_id = l.id
                         AND (v.order_id IS NULL OR v.order_id <> ALL (v_orders))
                         AND (v.payout_id IS NULL
                           OR EXISTS (SELECT 1 FROM public.payout_items i2 JOIN public.order_lines l2 ON l2.id = i2.order_line_id
                                       WHERE i2.payout_id = v.payout_id AND l2.order_id <> ALL (v_orders))))));
    EXIT WHEN cardinality(v_bad) = 0;
    v_orders := ARRAY(SELECT x FROM unnest(v_orders) x WHERE x <> ALL (v_bad));
  END LOOP;

  -- 3. The batch: the oldest p_limit orders, closed over the payouts and
  -- ledger transactions they share (all of them passed the fixpoint).
  IF p_dry_run THEN
    v_sel := v_orders;
  ELSE
    v_sel := ARRAY(SELECT o.id FROM public.orders o WHERE o.id = ANY (v_orders) ORDER BY o.created_at, o.id LIMIT p_limit);
    LOOP
      v_grow := ARRAY(
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.payout_items i ON i.order_line_id = l.id
          JOIN public.payout_items i2 ON i2.payout_id = i.payout_id
          JOIN public.order_lines l2 ON l2.id = i2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel)
        UNION
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.ledger_entries e ON e.order_line_id = l.id
          JOIN public.ledger_entries e2 ON e2.transaction_id = e.transaction_id
          JOIN public.order_lines l2 ON l2.id = e2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel)
        UNION
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.invoice_lines il ON il.order_line_id = l.id
          JOIN public.invoice_lines il2 ON il2.invoice_id = il.invoice_id
          JOIN public.order_lines l2 ON l2.id = il2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel)
        UNION
        SELECT v.order_id FROM public.order_lines l
          JOIN public.invoice_lines il ON il.order_line_id = l.id
          JOIN public.invoices v ON v.id = il.invoice_id
         WHERE l.order_id = ANY (v_sel) AND v.order_id IS NOT NULL AND v.order_id <> ALL (v_sel)
        UNION
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.invoice_lines il ON il.order_line_id = l.id
          JOIN public.invoices v ON v.id = il.invoice_id
          JOIN public.payout_items i2 ON i2.payout_id = v.payout_id
          JOIN public.order_lines l2 ON l2.id = i2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel));
      EXIT WHEN cardinality(v_grow) = 0;
      v_sel := v_sel || v_grow;
    END LOOP;
  END IF;
  v_lines := ARRAY(SELECT l.id FROM public.order_lines l WHERE l.order_id = ANY (v_sel));
  v_payouts := ARRAY(SELECT i.payout_id FROM public.payout_items i WHERE i.order_line_id = ANY (v_lines)
                     UNION SELECT l.payout_id FROM public.order_lines l WHERE l.id = ANY (v_lines) AND l.payout_id IS NOT NULL);
  v_txns := ARRAY(SELECT x.id FROM public.ledger_transactions x
                   WHERE (x.ref_type = 'order' AND x.ref_id = ANY (v_sel))
                      OR (x.ref_type = 'payment' AND x.ref_id IN (SELECT p.id FROM public.payments p WHERE p.order_id = ANY (v_sel)))
                      OR (x.ref_type = 'refund' AND x.ref_id IN (SELECT r.id FROM public.refunds r WHERE r.order_id = ANY (v_sel)))
                      OR (x.ref_type = 'payout' AND x.ref_id = ANY (v_payouts))
                      OR EXISTS (SELECT 1 FROM public.ledger_entries e WHERE e.transaction_id = x.id AND e.order_line_id = ANY (v_lines)));
  v_inv := ARRAY(WITH RECURSIVE inv (id) AS (
                    SELECT v.id FROM public.invoices v
                     WHERE v.order_id = ANY (v_sel) OR v.payout_id = ANY (v_payouts)
                        OR EXISTS (SELECT 1 FROM public.invoice_lines il WHERE il.invoice_id = v.id AND il.order_line_id = ANY (v_lines))
                    UNION
                    SELECT j.id FROM public.invoices j JOIN inv ON j.refers_to_invoice_id = inv.id)
                  SELECT inv.id FROM inv);

  IF p_dry_run THEN
    v_n := cardinality(v_sel) + cardinality(v_lines) + cardinality(v_payouts) + cardinality(v_txns) + cardinality(v_inv)
         + (SELECT count(*) FROM public.payments p WHERE p.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.payment_splits s WHERE s.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.refunds r WHERE r.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.incoming_bank_transfers t WHERE t.matched_order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.invoice_lines il WHERE il.invoice_id = ANY (v_inv))
         + (SELECT count(*) FROM public.ledger_entries e WHERE e.transaction_id = ANY (v_txns))
         + (SELECT count(*) FROM public.payout_items i WHERE i.payout_id = ANY (v_payouts));
  ELSIF cardinality(v_sel) > 0 THEN
    -- 4. Leaves first; the payout cycle (order_lines ↔ payout_items ↔
    -- payouts) goes in one statement, its foreign keys are checked at the
    -- statement's end.
    DELETE FROM public.invoice_lines WHERE invoice_id = ANY (v_inv);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.invoices WHERE id = ANY (v_inv);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', 'public.ledger_entries', true);
    DELETE FROM public.ledger_entries WHERE transaction_id = ANY (v_txns);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', 'public.ledger_transactions', true);
    DELETE FROM public.ledger_transactions WHERE id = ANY (v_txns);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', '', true);
    DELETE FROM public.payment_splits WHERE order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.refunds WHERE order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', 'public.payout_items', true);
    WITH di AS (DELETE FROM public.payout_items WHERE payout_id = ANY (v_payouts) RETURNING 1),
         dl AS (DELETE FROM public.order_lines WHERE id = ANY (v_lines) RETURNING 1),
         dp AS (DELETE FROM public.payouts WHERE id = ANY (v_payouts) RETURNING 1)
    SELECT (SELECT count(*) FROM di) + (SELECT count(*) FROM dl) + (SELECT count(*) FROM dp) INTO v_k;
    v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', '', true);
    DELETE FROM public.payments WHERE order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.incoming_bank_transfers WHERE matched_order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.orders WHERE id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
  END IF;

  -- 5. Loose and master rows nothing refers to any more.
  v_ids := ARRAY(SELECT p.id FROM public.payouts p
                  WHERE p.status IN ('failed','cancelled') AND greatest(p.created_at, p.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.payout_items i WHERE i.payout_id = p.id)
                    AND NOT EXISTS (SELECT 1 FROM public.order_lines l WHERE l.payout_id = p.id)
                    AND NOT EXISTS (SELECT 1 FROM public.invoices v WHERE v.payout_id = p.id)
                    AND NOT EXISTS (SELECT 1 FROM public.ledger_transactions x WHERE x.ref_type = 'payout' AND x.ref_id = p.id)
                  ORDER BY p.created_at, p.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.payouts WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT t.id FROM public.incoming_bank_transfers t
                  WHERE t.matched_order_id IS NULL AND greatest(t.created_at, t.updated_at) < v_cutoff
                  ORDER BY t.created_at, t.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.incoming_bank_transfers WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT v.id FROM public.invoices v
                  WHERE v.order_id IS NULL AND v.payout_id IS NULL AND greatest(v.created_at, v.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.invoices j WHERE j.refers_to_invoice_id = v.id)
                    AND NOT EXISTS (SELECT 1 FROM public.invoice_lines il WHERE il.invoice_id = v.id AND il.order_line_id IS NOT NULL)
                  ORDER BY v.created_at, v.id LIMIT v_limit);
  IF p_dry_run THEN
    v_n := v_n + cardinality(v_ids) + (SELECT count(*) FROM public.invoice_lines il WHERE il.invoice_id = ANY (v_ids));
  ELSE
    DELETE FROM public.invoice_lines WHERE invoice_id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.invoices WHERE id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
  END IF;

  v_ids := ARRAY(SELECT b.id FROM public.payee_bank_accounts b
                  WHERE b.status IN ('superseded','rejected','revoked') AND greatest(b.created_at, b.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.payouts p WHERE p.bank_account_id = b.id)
                    AND NOT EXISTS (SELECT 1 FROM public.payee_bank_accounts n WHERE n.superseded_by_id = b.id)
                  ORDER BY b.created_at, b.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.payee_bank_accounts WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT y.id FROM public.payees y
                  WHERE y.status IN ('draft','closed') AND greatest(y.created_at, y.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.payee_bank_accounts b WHERE b.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.offerings f WHERE f.default_payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.order_lines l WHERE l.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.ledger_entries e WHERE e.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.payouts p WHERE p.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.payout_items i WHERE i.payee_id = y.id)
                  ORDER BY y.created_at, y.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.payees WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT lp.id FROM public.legal_parties lp
                  WHERE lp.kind <> 'platform' AND greatest(lp.created_at, lp.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.legal_party_documents d
                                     WHERE d.legal_party_id = lp.id AND greatest(d.created_at, d.verified_at) >= v_cutoff)
                    AND NOT EXISTS (SELECT 1 FROM public.payees y WHERE y.legal_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.orders o WHERE o.buyer_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.invoices v WHERE v.issuer_party_id = lp.id OR v.recipient_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.collection_accounts c WHERE c.legal_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.provider_contact_map m WHERE m.legal_party_id = lp.id)
                  ORDER BY lp.created_at, lp.id LIMIT v_limit);
  IF p_dry_run THEN
    v_n := v_n + cardinality(v_ids) + (SELECT count(*) FROM public.legal_party_documents d WHERE d.legal_party_id = ANY (v_ids));
  ELSE
    DELETE FROM public.legal_party_documents WHERE legal_party_id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.legal_parties WHERE id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
  END IF;
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.financial_records_purge(timestamptz, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.financial_records_purge(timestamptz, integer, boolean) TO app_rw;

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
