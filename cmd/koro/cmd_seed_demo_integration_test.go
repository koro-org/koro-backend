//go:build integration

package main

import (
	"bytes"
	"regexp"
	"strconv"
	"testing"

	"koro/db/seed"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// demoPassword is the test password of the demo accounts (synthetic).
const demoPassword = "demo-parola-2026-koro"

// seedEnviron is the environment of `koro seed …` against d (the
// .env.example development placeholders).
func seedEnviron(t *testing.T, d *db.DB) map[string]string {
	t.Helper()
	return map[string]string{
		"APP_ENV":                "dev",
		"DATABASE_URL":           testutil.AppURL(t, d),
		"DB_MAX_CONNS":           "4",
		"REDIS_URL":              "redis://127.0.0.1:1/0", // seeding never connects to Redis
		"PASSWORD_PEPPER":        "devpepperdevpepperdevpepperdevpepper",
		"CRYPTO_KEKS":            "dev1:ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGU=",
		"CRYPTO_BLIND_INDEX_KEY": "aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWQ=",
		"CRYPTO_TOKEN_KEYS":      "dev1:dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8=",
	}
}

// seedCmd runs `koro seed <args>` and returns its output.
func seedCmd(t *testing.T, environ map[string]string, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"seed"}, args...), environ, &stdout, &stderr); code != 0 {
		t.Fatalf("koro seed %v: exit %d: %s", args, code, stderr.String())
	}
	return stdout.String()
}

var demoSummary = regexp.MustCompile(`demo seed: (\d+) rows inserted, (\d+) already there`)

// TestSeedDemo_Idempotent (BE-033): `koro seed demo` over the reference
// data fills an empty database and a second run inserts nothing (every
// row has its deterministic ID; ON CONFLICT DO NOTHING); the counts stay
// the same and the derived columns agree with the tables.
func TestSeedDemo_Idempotent(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	environ := seedEnviron(t, d)
	seedCmd(t, environ, "reference")
	summary := func(out string) (int, int) {
		t.Helper()
		m := demoSummary.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no summary in %q", out)
		}
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		return a, b
	}
	out := seedCmd(t, environ, "demo", "-password", demoPassword)
	inserted, existing := summary(out)
	if inserted < 3000 || existing != 0 {
		t.Fatalf("first run: %d inserted, %d existing\n%s", inserted, existing, out)
	}
	if bytes.Contains([]byte(out), []byte(demoPassword)) {
		t.Fatal("the output shows the password")
	}
	tables := []string{"users", "organizations", "clubs", "org_members", "student_profiles", "events", "event_registrations",
		"registration_tickets", "event_checkins", "posts", "post_reactions", "segments", "entitlements", "platform_role_assignments"}
	counts := func() map[string]int {
		out := map[string]int{}
		for _, tb := range tables {
			var n int
			if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM `+tb).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[tb] = n
		}
		return out
	}
	before := counts()
	again, existing := summary(seedCmd(t, environ, "demo", "-password", demoPassword))
	if again != 0 || existing != inserted {
		t.Fatalf("second run: %d inserted, %d existing (first run inserted %d)", again, existing, inserted)
	}
	after := counts()
	for _, tb := range tables {
		if before[tb] != after[tb] || before[tb] == 0 {
			t.Errorf("%s: %d rows, then %d", tb, before[tb], after[tb])
		}
	}
	// Derived columns agree with the rows (what the services maintain).
	var drift int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM events e WHERE e.confirmed_count <>
		(SELECT count(*) FROM event_registrations r WHERE r.event_id = e.id AND r.status IN ('confirmed','checked_in'))`).Scan(&drift); err != nil || drift != 0 {
		t.Fatalf("confirmed_count drift on %d events (%v)", drift, err)
	}
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM posts p WHERE p.like_count <>
		(SELECT count(*) FROM post_reactions r WHERE r.post_id = p.id)`).Scan(&drift); err != nil || drift != 0 {
		t.Fatalf("like_count drift on %d posts (%v)", drift, err)
	}
	var segment *int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count_segment(audience_keys, audience_dim_count) FROM segments s
		JOIN organizations o ON o.id = s.org_id WHERE o.slug = 'demo-demo-yazilim'`).Scan(&segment); err != nil || segment == nil || *segment < 10 {
		t.Fatalf("company segment count = %v (%v), want ≥ 10", segment, err)
	}
	var minors int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE signup_persona = 'student'
		AND birth_date > (current_date - interval '18 years')`).Scan(&minors); err != nil || minors < 10 {
		t.Fatalf("minors = %d (%v)", minors, err)
	}
	var withPassword int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE email = $1 AND password_hash LIKE '$argon2id$%'`,
		seed.StudentEmail).Scan(&withPassword); err != nil || withPassword != 1 {
		t.Fatalf("demo student password = %d (%v)", withPassword, err)
	}
}
