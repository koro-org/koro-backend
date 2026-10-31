//go:build integration

package migrations_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// Tests of 000172 (BE-164; SR §6): the product KPI functions, their k ≥ 10
// suppression and their analytics views.

// verifiedStudent inserts an active student with a valid .edu.tr
// verification and an active profile at the university.
func (w *p1aWorld) verifiedStudent(uni uuid.UUID) uuid.UUID {
	w.t.Helper()
	s := w.student()
	w.eduVerification(s, uni, "verified", "now() - interval '10 days'", "now() + interval '355 days'")
	w.profile(s, uni, true, "all")
	return s
}

// eduVerification inserts a verification of the user in the given status.
func (w *p1aWorld) eduVerification(user, uni uuid.UUID, status, verifiedAt, expiresAt string) {
	w.t.Helper()
	email := "m" + w.n() + "@ogr.example.edu.tr"
	w.exec(`INSERT INTO edu_verifications (user_id, method, edu_email, edu_email_normalized, email_domain, status, university_id,
		verified_at, expires_at) VALUES ($1, 'edu_email', $2, $2, 'ogr.example.edu.tr', $3, $4, `+verifiedAt+`, `+expiresAt+`)`,
		user, email, status, uni)
}

// metricRows reads a metrics function as "metric|numerator|denominator|value|suppressed" (NULL as "-").
func metricRows(t *testing.T, w *p1aWorld, from string) []string {
	t.Helper()
	return roRows(t, appRO(t, w), `SELECT metric, numerator::text, denominator::text, value::text, suppressed::text FROM `+from)
}

// TestProductMetrics_Definitions_KAnonymity (BE-164, SR §6): every metric
// on a small world built to hit each rule: verification at the moment of
// the interaction, check-in only events, accepted ÷ sent, engagement of
// counted dispatches only, organisation counts unsuppressed, sensitive
// clubs out of the member average, and every people count of 1..9 NULL.
func TestProductMetrics_Definitions_KAnonymity(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni, _ := w.university()
	creator := w.user()
	club := w.club(uni)
	w.exec(`UPDATE clubs SET claim_status = 'claimed' WHERE org_id = $1`, club)
	sensitive := w.club(uni)
	w.exec(`UPDATE clubs SET claim_status = 'claimed', category = 'religious', is_sensitive = true, sensitive_reason = 'religion',
		sensitivity_set_at = now() WHERE org_id = $1`, sensitive)
	w.club(uni) // unclaimed
	member := func(org, user uuid.UUID) {
		w.exec(`INSERT INTO org_members (org_id, org_kind, user_id, role, status, source, joined_at)
			VALUES ($1, 'club', $2, 'member', 'active', 'admin', now() - interval '1 day')`, org, user)
	}

	event := w.event(club, creator, map[string]string{"checkin_enabled": "true",
		"starts_at": "now() - interval '1 hour'", "ends_at": "now() + interval '1 hour'"})
	noCheckin := w.event(club, creator, map[string]string{"starts_at": "now() - interval '1 hour'", "ends_at": "now() + interval '1 hour'"})
	dispatch := func(counted bool) uuid.UUID {
		id, post := uuid.NewV7(), w.post(club, "club", creator, nil)
		sent := "12"
		if !counted {
			sent = "NULL"
		}
		w.exec(`INSERT INTO outreach_dispatches (id, org_id, post_id, audience_keys, audience_dim_count, channels, title_snapshot,
			deep_link, commercial, status, sent_count, started_at, completed_at, created_by)
			VALUES ($1, $2, $3, ARRAY['all'], 1, ARRAY['in_app'], 'Duyuru', '/posts/x', false, 'completed', `+sent+`,
			now() - interval '1 minute', now(), $4)`, id, club, post, creator)
		return id
	}
	counted, uncounted := dispatch(true), dispatch(false)
	deliver := func(d, user uuid.UUID, status string, read bool) {
		w.exec(`INSERT INTO notification_deliveries (user_id, channel, category, template, template_version, dedupe_key, status)
			VALUES ($1, 'in_app', 'transactional', 'outreach.dispatch', 1, $2, $3)`, user, "dispatch:"+d.String()+":"+user.String()+":in_app", status)
		readAt := "NULL"
		if read {
			readAt = "now()"
		}
		w.exec(`INSERT INTO in_app_notifications (user_id, kind, title, data, read_at) VALUES ($1, 'outreach.dispatch', 'Duyuru',
			jsonb_build_object('dispatch_id', $2::text), `+readAt+`)`, user, d.String())
	}

	// 12 verified students: 10 checked in, 2 no-shows; 11 seen in 30 days;
	// 10 discoverable; members of the claimed club (12) and of the
	// sensitive one (10); 12 reached by the counted dispatch, 10 opened it.
	for i := range 12 {
		s := w.verifiedStudent(uni)
		over := map[string]string{"status": "'no_show'"}
		if i < 10 {
			over = map[string]string{"status": "'checked_in'", "checked_in_at": "now()"}
		}
		w.registration(event, club, s, over)
		w.registration(noCheckin, club, s, nil)
		if i < 11 {
			w.exec(`INSERT INTO sessions (user_id, token_hash, client_kind, idle_expires_at, absolute_expires_at, last_seen_at)
				VALUES ($1, $2, 'web', now() + interval '7 days', now() + interval '30 days', now() - interval '1 day')`, s, bytes32("pm"+w.n()))
		}
		w.exec(`INSERT INTO student_privacy_settings (user_id, discoverable) VALUES ($1, $2)`, s, i < 10)
		member(club, s)
		if i < 10 {
			member(sensitive, s)
		}
		deliver(counted, s, "sent", i < 10)
		deliver(uncounted, s, "sent", true)
	}
	// A 13th delivery that failed is not a reach.
	failed := w.verifiedStudent(uni)
	w.exec(`INSERT INTO notification_deliveries (user_id, channel, category, template, template_version, dedupe_key, status)
		VALUES ($1, 'push', 'transactional', 'outreach.dispatch', 1, $2, 'failed')`, failed, "dispatch:"+counted.String()+":"+failed.String()+":push")
	// Two stale profiles (verification expired 13 months after it was
	// given), one of them edited this year.
	for i := range 2 {
		s := w.student()
		w.eduVerification(s, uni, "expired", "now() - interval '13 months'", "now() - interval '1 month'")
		w.profile(s, uni, false, "all")
		if i == 0 {
			w.exec(`INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 'en', 'B2')`, s)
		}
	}

	// 5 contact requests, one accepted by a student whose verification was
	// revoked before (not verified at the interaction: no north star).
	company := w.org("company")
	revoked := w.student()
	w.eduVerification(revoked, uni, "revoked", "now() - interval '10 days'", "now() + interval '355 days'")
	for i := range 5 {
		status, responded, to := "'pending'", "NULL", w.student()
		if i == 0 {
			status, responded, to = "'accepted'", "clock_timestamp()", revoked
		}
		w.exec(`INSERT INTO contact_requests (org_id, sender_user_id, student_user_id, via, subject, message, status, responded_at,
			expires_at, entitlement_usage_id) VALUES ($1, $2, $3, 'discovery', 'Staj', 'Staj programımız hakkında', `+status+`, `+responded+`,
			now() + interval '14 days', $4)`, company, creator, to, w.usage(company, "contact_request"))
	}

	// Commerce: one paid order; one renewed package and one that ended.
	w.markPaid(func() uuid.UUID { o, _ := w.order(w.shop()); return o }())
	renewed, lapsed := w.org("company"), w.org("company")
	ended := map[string]string{"status": "'expired'", "valid_from": "now() - interval '31 days'",
		"valid_to": "now() - interval '1 hour'", "created_at": "now() - interval '31 days'"}
	w.entitlement(renewed, ended)
	w.entitlement(renewed, map[string]string{"valid_from": "now() - interval '1 hour'", "valid_to": "now() + interval '29 days'"})
	w.entitlement(lapsed, ended)

	period := metricRows(t, w, `public.koro_metrics_period(now() - interval '1 day', now() + interval '1 hour')`)
	if want := []string{
		"engaged_verified_students|10|-|10|false",            // the revoked student's acceptance does not count
		"event_attendance_rate|10|12|0.8333|false",           // the event without check-in is left out
		"contact_request_acceptance_rate|-|-|-|true",         // 1 of 5: both below 10
		"targeted_post_engagement_rate|10|12|0.8333|false",   // uncounted dispatch and failed delivery out
		"paying_companies|1|-|1|false",                       // organisations: not suppressed
		"revenue_per_paying_company|1200000|1|1200000|false", // kuruş
		"package_renewal_rate|1|2|0.5000|false",
	}; !slices.Equal(period, want) {
		t.Errorf("period metrics:\n got %q\nwant %q", period, want)
	}
	current := metricRows(t, w, `public.koro_metrics_current(now())`)
	if want := []string{
		"verified_active_students|11|13|11|false",           // 13 verified (the failed-delivery student too), 11 seen
		"data_freshness_rate|14|15|0.9333|false",            // 13 verified + 1 edited of 15 profiles
		"verified_clubs|2|4|2|false",                        // claimed incl. the sensitive one; 4 active clubs (the shop seller is one)
		"active_members_per_verified_club|12|1|12.00|false", // the sensitive club's members left out
		"discoverability_rate|10|15|0.6667|false",
	}; !slices.Equal(current, want) {
		t.Errorf("current metrics:\n got %q\nwant %q", current, want)
	}

	// The same rows through the analytics views (Metabase).
	if got := metricRows(t, w, `analytics.product_metrics_current`); !slices.Equal(got, current) {
		t.Errorf("analytics.product_metrics_current = %q", got)
	}
	monthly := roRows(t, appRO(t, w), `SELECT count(DISTINCT month)::int, count(*)::int FROM analytics.product_metrics_monthly`)
	if !slices.Equal(monthly, []string{"24|168"}) {
		t.Errorf("product_metrics_monthly months|rows = %v, want 24 months × 7 metrics", monthly)
	}

	// The nightly snapshot (app_rw) writes the day once; a rerun overwrites.
	for range 2 {
		w.exec(`INSERT INTO product_metrics_daily (day, metric, unit, numerator, denominator, value, suppressed, taken_at)
			SELECT current_date, m.metric, m.unit, m.numerator, m.denominator, m.value, m.suppressed, now()
			  FROM public.koro_metrics_current(now()) m
			ON CONFLICT (day, metric) DO UPDATE SET value = EXCLUDED.value, taken_at = EXCLUDED.taken_at`)
	}
	if got := roRows(t, appRO(t, w), `SELECT count(*)::int FROM analytics.product_metrics_daily`); !slices.Equal(got, []string{"5"}) {
		t.Errorf("product_metrics_daily rows = %v, want 5", got)
	}
}

// TestProductMetrics_Grants (BE-164): app_rw and app_ro execute the metric
// functions, PUBLIC does not; app_rw writes the snapshot table but cannot
// read the analytics views; the helpers are not callable by app_rw.
func TestProductMetrics_Grants(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	for _, q := range []string{`SELECT * FROM public.koro_metrics_period(now() - interval '30 days', now())`,
		`SELECT * FROM public.koro_metrics_current(now())`} {
		if got := execState(t, w.d.Pool, q); got != "" {
			t.Errorf("app_rw %s: SQLSTATE %q", q, got)
		}
		if _, err := appRO(t, w).Exec(t.Context(), q); err != nil {
			t.Errorf("app_ro %s: %v", q, err)
		}
	}
	for _, q := range []string{`SELECT public.koro_student_verified_at(gen_random_uuid(), now())`,
		`SELECT * FROM public.koro_metric('x', 'count', 1, NULL, true, false)`, `SELECT * FROM analytics.product_metrics_current`} {
		if got := execState(t, w.d.Pool, q); got != db.CodeInsufficientPriv {
			t.Errorf("app_rw %s: SQLSTATE %q, want %s", q, got, db.CodeInsufficientPriv)
		}
	}
	if got := execState(t, appRO(t, w), `SELECT * FROM public.product_metrics_daily`); got != db.CodeInsufficientPriv {
		t.Errorf("app_ro reads the snapshot table directly: SQLSTATE %q", got)
	}
}
