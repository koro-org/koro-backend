-- 000172_create_product_metrics.up.sql (BE-164; SR §6, uyum denetimi #75).
-- The product KPIs of SR §6 ("kuzey yıldızı" and the metric table), defined
-- once in SQL and read by both the admin API (GET /admin/metrics, app_rw)
-- and Metabase (analytics.*, app_ro):
--
-- * public.koro_metrics_period(from, to): flow metrics of [from, to)
--   (north star, attendance, contact acceptance, targeted-post engagement,
--   commercial metrics). public.koro_metrics_current(at): stock metrics as
--   of at (verified active students, data freshness, verified clubs and
--   their members, discoverability). Both return one row per metric in a
--   fixed order: metric, unit (count | ratio | average | kurus), numerator,
--   denominator, value and suppressed.
-- * k >= 10 (01 §1.4, as 000121): a count of people (or of rows about
--   people) between 1 and 9 is NULL, never the real number, and suppressed
--   is true; a value built from it is NULL. Counts of organisations and
--   amounts are not suppressed. The output never holds a person's ID, name
--   or timestamp: aggregates only.
-- * SECURITY DEFINER (owner app_owner, fixed search_path): the functions
--   read the source tables with the owner's privileges, so app_ro (which
--   reads no table) gets the aggregates through the analytics views, and
--   the functions never return anything finer than those views do.
--   EXECUTE: app_rw (the API, the nightly snapshot) and app_ro (Metabase).
-- * product_metrics_daily: the nightly snapshot of the stock metrics
--   (kvkk.metrics_snapshot), so that their trend survives (sessions and
--   verifications are purged after their retention). No personal data
--   (02 §0.6 "yok"): one row per day and metric.
-- * analytics.product_metrics_monthly (flow metrics, the last 24 months in
--   Europe/Istanbul), analytics.product_metrics_current and
--   analytics.product_metrics_daily: security_barrier views for app_ro.
--
-- Definitions (the doc table of SR §6 lives in the BE-164 ticket):
--   engaged_verified_students  distinct students verified at the moment of
--       at least one meaningful interaction in the period: a check-in, an
--       accepted contact request (responded_at), or an application (a
--       registration to an event with registration_mode = 'manual').
--   event_attendance_rate  checked_in ÷ (confirmed + checked_in + no_show)
--       registrations of the events starting in the period that use
--       check-in (checkin_enabled), published or completed.
--   contact_request_acceptance_rate  accepted ÷ all contact requests sent
--       in the period (current status: pending ones count as not accepted).
--   targeted_post_engagement_rate  (dispatch, student) pairs reached by a
--       targeted dispatch started in the period (a sent/delivered
--       notification_deliveries row of the dispatch, any channel) that
--       either opened the in-app notification (read_at) or registered for
--       the shared event after the dispatch started ÷ reached pairs.
--       Dispatches without published counts (a sensitive club's members)
--       are left out. E-mail and push clicks are not tracked.
--   paying_companies  companies with an order paid in the period.
--   revenue_per_paying_company  Σ (total_amount − refunded_amount) of the
--       orders paid in the period ÷ paying companies, in kuruş (gross order
--       value incl. VAT, not Koro's commission).
--   package_renewal_rate  companies whose purchased entitlement
--       (source_type = 'order_line') ended (valid_to) in the period and that
--       got a new purchased entitlement starting within 30 days of that end
--       ÷ companies with such an ending entitlement.
--   verified_active_students  students with a valid verification and a
--       session seen in the 30 days before at (denominator: students with a
--       valid verification).
--   data_freshness_rate  active student profiles verified (verified_at) or
--       edited (skills, languages, experiences, CV) in the 12 months before
--       at ÷ active student profiles. student_profiles.updated_at is not
--       used: the audience reconcile of students also bumps it.
--   verified_clubs  active clubs whose management is approved
--       (claim_status = 'claimed') (denominator: active clubs).
--   active_members_per_verified_club  active members ÷ verified clubs,
--       sensitive clubs left out (KVKK md. 6).
--   discoverability_rate  active student profiles with discoverable = true
--       (consent discovery_profile) ÷ active student profiles.
BEGIN;
SET LOCAL lock_timeout = '5s';

-- One metric row: k >= 10 on the operands that count people (p_num_people,
-- p_den_people), then the value.
CREATE FUNCTION public.koro_metric(p_metric text, p_unit text, p_num bigint, p_den bigint, p_num_people boolean, p_den_people boolean,
  OUT metric text, OUT unit text, OUT numerator bigint, OUT denominator bigint, OUT value numeric, OUT suppressed boolean)
  LANGUAGE sql IMMUTABLE AS $$
  SELECT p_metric, p_unit, s.n, s.d,
         CASE WHEN p_unit = 'count' THEN s.n::numeric
              WHEN s.n IS NULL OR s.d IS NULL OR s.d = 0 THEN NULL
              WHEN p_unit = 'ratio' THEN round(s.n::numeric / s.d, 4)
              WHEN p_unit = 'average' THEN round(s.n::numeric / s.d, 2)
              ELSE round(s.n::numeric / s.d, 0) END,
         (s.n IS NULL AND p_num IS NOT NULL) OR (s.d IS NULL AND p_den IS NOT NULL)
    FROM (SELECT CASE WHEN p_num_people AND p_num BETWEEN 1 AND 9 THEN NULL ELSE p_num END AS n,
                 CASE WHEN p_den_people AND p_den BETWEEN 1 AND 9 THEN NULL ELSE p_den END AS d) s $$;
REVOKE ALL ON FUNCTION public.koro_metric(text, text, bigint, bigint, boolean, boolean) FROM PUBLIC;

-- Whether the user held a valid student verification at p_at: verified
-- before it, not expired, not yet superseded or revoked (updated_at is the
-- moment of that change).
CREATE FUNCTION public.koro_student_verified_at(p_user uuid, p_at timestamptz) RETURNS boolean
  LANGUAGE sql STABLE SET search_path = pg_catalog, public AS $$
  SELECT EXISTS (SELECT 1 FROM public.edu_verifications v
                  WHERE v.user_id = p_user AND v.verified_at <= p_at AND v.expires_at > p_at
                    AND (v.status IN ('verified','expired')
                         OR (v.status IN ('superseded','revoked') AND v.updated_at > p_at))) $$;
REVOKE ALL ON FUNCTION public.koro_student_verified_at(uuid, timestamptz) FROM PUBLIC;

CREATE FUNCTION public.koro_metrics_period(p_from timestamptz, p_to timestamptz)
  RETURNS TABLE (metric text, unit text, numerator bigint, denominator bigint, value numeric, suppressed boolean)
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public AS $$
  WITH interactions AS (
    SELECT r.user_id, r.checked_in_at AS at FROM public.event_registrations r
     WHERE r.status = 'checked_in' AND r.checked_in_at >= p_from AND r.checked_in_at < p_to
    UNION ALL
    SELECT c.student_user_id, c.responded_at FROM public.contact_requests c
     WHERE c.status = 'accepted' AND c.responded_at >= p_from AND c.responded_at < p_to
    UNION ALL
    SELECT r.user_id, r.created_at FROM public.event_registrations r JOIN public.events e ON e.id = r.event_id
     WHERE e.registration_mode = 'manual' AND r.created_at >= p_from AND r.created_at < p_to
  ), engaged AS (
    SELECT count(DISTINCT i.user_id) AS n FROM interactions i WHERE public.koro_student_verified_at(i.user_id, i.at)
  ), attendance AS (
    SELECT count(*) FILTER (WHERE r.status = 'checked_in') AS n, count(*) AS d
      FROM public.event_registrations r JOIN public.events e ON e.id = r.event_id
     WHERE e.checkin_enabled AND e.status IN ('published','completed')
       AND e.starts_at >= p_from AND e.starts_at < p_to
       AND r.status IN ('confirmed','checked_in','no_show')
  ), contacts AS (
    SELECT count(*) FILTER (WHERE c.status = 'accepted') AS n, count(*) AS d
      FROM public.contact_requests c WHERE c.created_at >= p_from AND c.created_at < p_to
  ), dispatches AS (
    SELECT d.id, d.post_id, d.started_at FROM public.outreach_dispatches d
     WHERE d.sent_count IS NOT NULL AND d.started_at >= p_from AND d.started_at < p_to
  ), reached AS (
    -- notification_deliveries.dedupe_key of a dispatch is
    -- 'dispatch:<dispatch id>:<user id>:<channel>' (outreach dedupeKey).
    SELECT DISTINCT d.id AS dispatch_id, d.post_id, d.started_at, nd.user_id
      FROM public.notification_deliveries nd
      JOIN dispatches d ON nd.dedupe_key LIKE 'dispatch:%' AND split_part(nd.dedupe_key, ':', 2) = d.id::text
     WHERE nd.status IN ('sent','delivered') AND nd.user_id IS NOT NULL AND nd.created_at >= p_from
  ), engagement AS (
    SELECT count(*) FILTER (WHERE
             EXISTS (SELECT 1 FROM public.in_app_notifications n
                      WHERE n.user_id = x.user_id AND n.read_at IS NOT NULL AND n.data->>'dispatch_id' = x.dispatch_id::text)
             OR EXISTS (SELECT 1 FROM public.posts p JOIN public.event_registrations r ON r.event_id = p.event_id
                         WHERE p.id = x.post_id AND r.user_id = x.user_id AND r.created_at >= x.started_at)) AS n,
           count(*) AS d
      FROM reached x
  ), paid AS (
    SELECT count(DISTINCT o.org_id) AS companies, coalesce(sum(o.total_amount - o.refunded_amount), 0)::bigint AS amount
      FROM public.orders o WHERE o.paid_at >= p_from AND o.paid_at < p_to
  ), renewal AS (
    SELECT count(DISTINCT e.org_id) FILTER (WHERE EXISTS (
             SELECT 1 FROM public.entitlements x
              WHERE x.org_id = e.org_id AND x.id <> e.id AND x.source_type = 'order_line'
                AND x.status NOT IN ('pending','revoked') AND x.created_at > e.created_at
                AND x.valid_from >= e.valid_to - interval '30 days' AND x.valid_from < e.valid_to + interval '30 days')) AS n,
           count(DISTINCT e.org_id) AS d
      FROM public.entitlements e
     WHERE e.source_type = 'order_line' AND e.status NOT IN ('pending','revoked')
       AND e.valid_to >= p_from AND e.valid_to < p_to
  )
  SELECT m.metric, m.unit, m.numerator, m.denominator, m.value, m.suppressed
    FROM (VALUES
      (1, 'engaged_verified_students', 'count', (SELECT n FROM engaged), NULL::bigint, true, false),
      (2, 'event_attendance_rate', 'ratio', (SELECT n FROM attendance), (SELECT d FROM attendance), true, true),
      (3, 'contact_request_acceptance_rate', 'ratio', (SELECT n FROM contacts), (SELECT d FROM contacts), true, true),
      (4, 'targeted_post_engagement_rate', 'ratio', (SELECT n FROM engagement), (SELECT d FROM engagement), true, true),
      (5, 'paying_companies', 'count', (SELECT companies FROM paid), NULL::bigint, false, false),
      (6, 'revenue_per_paying_company', 'kurus', (SELECT amount FROM paid), (SELECT companies FROM paid), false, false),
      (7, 'package_renewal_rate', 'ratio', (SELECT n FROM renewal), (SELECT d FROM renewal), false, false)
    ) AS v(ord, code, unit, n, d, n_people, d_people)
    CROSS JOIN LATERAL public.koro_metric(v.code, v.unit, v.n, v.d, v.n_people, v.d_people) m
   ORDER BY v.ord $$;
REVOKE ALL ON FUNCTION public.koro_metrics_period(timestamptz, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.koro_metrics_period(timestamptz, timestamptz) TO app_rw, app_ro;

CREATE FUNCTION public.koro_metrics_current(p_at timestamptz)
  RETURNS TABLE (metric text, unit text, numerator bigint, denominator bigint, value numeric, suppressed boolean)
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public AS $$
  WITH students AS (
    SELECT sp.user_id FROM public.student_profiles sp JOIN public.users u ON u.id = sp.user_id
     WHERE sp.status = 'active' AND u.status = 'active' AND sp.created_at <= p_at
  ), verified AS (
    SELECT s.user_id FROM students s WHERE public.koro_student_verified_at(s.user_id, p_at)
  ), active AS (
    SELECT count(*) FILTER (WHERE EXISTS (SELECT 1 FROM public.sessions se
                                           WHERE se.user_id = v.user_id AND se.created_at <= p_at
                                             AND se.last_seen_at > p_at - interval '30 days')) AS n,
           count(*) AS d
      FROM verified v
  ), fresh AS (
    SELECT count(*) FILTER (WHERE
             EXISTS (SELECT 1 FROM public.edu_verifications v
                      WHERE v.user_id = s.user_id AND v.status IN ('verified','expired','superseded')
                        AND v.verified_at > p_at - interval '12 months' AND v.verified_at <= p_at)
             OR EXISTS (SELECT 1 FROM public.student_skills k WHERE k.user_id = s.user_id
                         AND k.created_at > p_at - interval '12 months' AND k.created_at <= p_at)
             OR EXISTS (SELECT 1 FROM public.student_languages l WHERE l.user_id = s.user_id
                         AND l.created_at > p_at - interval '12 months' AND l.created_at <= p_at)
             OR EXISTS (SELECT 1 FROM public.student_experiences x WHERE x.user_id = s.user_id
                         AND x.updated_at > p_at - interval '12 months' AND x.updated_at <= p_at)
             OR EXISTS (SELECT 1 FROM public.student_cvs c WHERE c.user_id = s.user_id
                         AND c.created_at > p_at - interval '12 months' AND c.created_at <= p_at)) AS n,
           count(*) AS d
      FROM students s
  ), clubs AS (
    SELECT count(*) FILTER (WHERE c.claim_status = 'claimed') AS verified, count(*) AS all_clubs,
           count(*) FILTER (WHERE c.claim_status = 'claimed' AND NOT c.is_sensitive) AS verified_open
      FROM public.clubs c JOIN public.organizations o ON o.id = c.org_id
     WHERE o.status = 'active' AND c.created_at <= p_at
  ), members AS (
    SELECT count(*) AS n
      FROM public.org_members m JOIN public.clubs c ON c.org_id = m.org_id JOIN public.organizations o ON o.id = c.org_id
     WHERE m.status = 'active' AND m.joined_at <= p_at AND c.claim_status = 'claimed' AND NOT c.is_sensitive
       AND o.status = 'active' AND c.created_at <= p_at
  ), discover AS (
    SELECT count(*) FILTER (WHERE ps.discoverable) AS n, count(*) AS d
      FROM students s LEFT JOIN public.student_privacy_settings ps ON ps.user_id = s.user_id
  )
  SELECT m.metric, m.unit, m.numerator, m.denominator, m.value, m.suppressed
    FROM (VALUES
      (1, 'verified_active_students', 'count', (SELECT n FROM active), (SELECT d FROM active), true, true),
      (2, 'data_freshness_rate', 'ratio', (SELECT n FROM fresh), (SELECT d FROM fresh), true, true),
      (3, 'verified_clubs', 'count', (SELECT verified FROM clubs), (SELECT all_clubs FROM clubs), false, false),
      (4, 'active_members_per_verified_club', 'average', (SELECT n FROM members), (SELECT verified_open FROM clubs), true, false),
      (5, 'discoverability_rate', 'ratio', (SELECT n FROM discover), (SELECT d FROM discover), true, true)
    ) AS v(ord, code, unit, n, d, n_people, d_people)
    CROSS JOIN LATERAL public.koro_metric(v.code, v.unit, v.n, v.d, v.n_people, v.d_people) m
   ORDER BY v.ord $$;
REVOKE ALL ON FUNCTION public.koro_metrics_current(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.koro_metrics_current(timestamptz) TO app_rw, app_ro;

-- The nightly snapshot of the stock metrics (kvkk.metrics_snapshot): one
-- row per Europe/Istanbul day and metric; a rerun of the day overwrites.
CREATE TABLE product_metrics_daily (
  day          date NOT NULL,
  metric       text NOT NULL CONSTRAINT product_metrics_daily_metric_chk CHECK (metric ~ '^[a-z_]{1,60}$'),
  unit         text NOT NULL CONSTRAINT product_metrics_daily_unit_chk CHECK (unit IN ('count','ratio','average','kurus')),
  numerator    bigint,
  denominator  bigint,
  value        numeric,
  suppressed   boolean NOT NULL,
  taken_at     timestamptz NOT NULL,
  CONSTRAINT product_metrics_daily_pkey PRIMARY KEY (day, metric)
);
GRANT SELECT, INSERT, UPDATE ON product_metrics_daily TO app_rw;

-- analytics.* for Metabase (app_ro only, as 000121).
CREATE VIEW analytics.product_metrics_monthly WITH (security_barrier = true) AS
  SELECT mo.month::date AS month,
         (mo.month + interval '1 month') <= (now() AT TIME ZONE 'Europe/Istanbul') AS complete,
         m.metric, m.unit, m.numerator, m.denominator, m.value, m.suppressed
    FROM generate_series(date_trunc('month', now() AT TIME ZONE 'Europe/Istanbul') - interval '23 months',
                         date_trunc('month', now() AT TIME ZONE 'Europe/Istanbul'), interval '1 month') AS mo(month)
    CROSS JOIN LATERAL public.koro_metrics_period(mo.month AT TIME ZONE 'Europe/Istanbul',
                                                  (mo.month + interval '1 month') AT TIME ZONE 'Europe/Istanbul') m;

CREATE VIEW analytics.product_metrics_current WITH (security_barrier = true) AS
  SELECT m.metric, m.unit, m.numerator, m.denominator, m.value, m.suppressed
    FROM public.koro_metrics_current(now()) m;

CREATE VIEW analytics.product_metrics_daily WITH (security_barrier = true) AS
  SELECT d.day, d.metric, d.unit, d.numerator, d.denominator, d.value, d.suppressed
    FROM public.product_metrics_daily d;

REVOKE ALL ON analytics.product_metrics_monthly, analytics.product_metrics_current, analytics.product_metrics_daily FROM PUBLIC;
GRANT SELECT ON analytics.product_metrics_monthly, analytics.product_metrics_current, analytics.product_metrics_daily TO app_ro;
COMMIT;
