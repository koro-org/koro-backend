//go:build integration

package migrations_test

import (
	"strconv"
	"testing"
	"uuid"

	"koro/internal/platform/testutil"
)

// Tests of 000148 (BE-118, Q-17): the terms of use acceptance ledger.

// termsNotice inserts a published notice of kind and audience with body
// and returns its id and body digest (one current notice per kind and
// audience: notice_versions_current_uq).
func (w *p1aWorld) termsNotice(kind, audience, body string) (uuid.UUID, []byte) {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO notice_versions (id, kind, audience, version, title, body_md, body_sha256, published_at)
		VALUES ($1, $2, $3, $4, 'Metin', $5, $6, now())`, id, kind, audience, "2026.10.01-"+w.n(), body, sha(body))
	return id, sha(body)
}

// TestSchema_TermsAcceptances: an acceptance points to a terms_of_use row
// with that very body digest (composite FK), once per user and version;
// the CHECKs hold; the ledger is append-only for everyone, the owner
// included.
func TestSchema_TermsAcceptances(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	terms, sum := w.termsNotice("terms_of_use", "all", "Kullanım koşulları")
	privacy, psum := w.termsNotice("privacy_notice", "all", "Aydınlatma metni")
	u := w.user()
	ins := `INSERT INTO terms_acceptances (user_id, notice_version_id, text_sha256, channel, request_id, accepted_at)
		VALUES ($1, $2, $3, $4, $5, now())`
	expectOK(t, w.d.Pool, []p1aCase{{name: "web acceptance", sql: ins, args: []any{u, terms, sum, "web", "req-BE118-1"}}})
	expectFail(t, w.d.Pool, []p1aCase{
		{name: "same version twice", sql: ins, args: []any{u, terms, sum, "mobile", nil}, state: "23505", constraint: "terms_acceptances_user_notice_uq"},
		{name: "a privacy notice", sql: ins, args: []any{w.user(), privacy, psum, "web", nil}, state: "23503", constraint: "terms_acceptances_notice_fkey"},
		{name: "another digest", sql: ins, args: []any{w.user(), terms, psum, "web", nil}, state: "23503", constraint: "terms_acceptances_notice_fkey"},
		{name: "unknown user", sql: ins, args: []any{uuid.NewV7(), terms, sum, "web", nil}, state: "23503", constraint: "terms_acceptances_user_id_fkey"},
		{name: "channel", sql: ins, args: []any{w.user(), terms, sum, "admin", nil}, state: "23514", constraint: "terms_acceptances_channel_chk"},
		{name: "request id", sql: ins, args: []any{w.user(), terms, sum, "web", "has space"}, state: "23514", constraint: "terms_acceptances_request_id_chk"},
		{name: "kind", sql: `INSERT INTO terms_acceptances (user_id, notice_version_id, notice_kind, text_sha256, channel, accepted_at)
			VALUES ($1, $2, 'privacy_notice', $3, 'web', now())`, args: []any{w.user(), privacy, psum}, state: "23514",
			constraint: "terms_acceptances_notice_kind_chk"},
		{name: "update", sql: `UPDATE terms_acceptances SET channel = 'mobile' WHERE user_id = $1`, args: []any{u}, state: "42501"},
		{name: "delete", sql: `DELETE FROM terms_acceptances WHERE user_id = $1`, args: []any{u}, state: "42501"},
	})
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, owner, []p1aCase{
		{name: "owner update", sql: `UPDATE terms_acceptances SET channel = 'mobile' WHERE user_id = $1`, args: []any{u}, state: "23001"},
		{name: "owner delete", sql: `DELETE FROM terms_acceptances WHERE user_id = $1`, args: []any{u}, state: "23001"},
		{name: "owner truncate", sql: `TRUNCATE terms_acceptances`, state: "23001"},
	})
	if n := w.count(`SELECT count(*) FROM terms_acceptances`); n != 1 {
		t.Fatalf("acceptances = %d, want 1", n)
	}
}

// TestSchema_TermsAcceptancesPurge (terms_acceptance, 3 years after the
// erasure): only the acceptances of accounts erased before the cut-off go,
// oldest first; a live account keeps every version; unapproved, only the
// dry run works. The function is SECURITY DEFINER, owned by app_owner and
// executable by app_rw only.
func TestSchema_TermsAcceptancesPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	v1, s1 := w.termsNotice("terms_of_use", "all", "Koşullar v1")
	v2, s2 := w.termsNotice("terms_of_use", "student", "Koşullar v2")
	live, gone, recent := w.user(), w.user(), w.user()
	for i, r := range []struct {
		user   uuid.UUID
		notice uuid.UUID
		sum    []byte
	}{{live, v1, s1}, {live, v2, s2}, {gone, v1, s1}, {gone, v2, s2}, {recent, v1, s1}} {
		w.exec(`INSERT INTO terms_acceptances (user_id, notice_version_id, text_sha256, channel, accepted_at)
			VALUES ($1, $2, $3, 'web', now() - $4::interval)`, r.user, r.notice, r.sum, strconv.Itoa(10-i)+" years")
	}
	owner := testutil.OwnerConn(t, w.d)
	erase := func(id uuid.UUID, ago string) {
		t.Helper()
		if _, err := owner.Exec(t.Context(), `UPDATE users SET status = 'erased', email = NULL, email_normalized = NULL, password_hash = NULL,
			first_name = NULL, last_name = NULL, birth_date = NULL, erased_at = now() - $2::interval WHERE id = $1`, id, ago); err != nil {
			t.Fatal(err)
		}
	}
	erase(gone, "4 years")
	erase(recent, "1 year")

	if n, state := w.purge("terms_acceptances_purge", 1000, true); state != "" || n != 2 {
		t.Fatalf("dry run = %d, %q; want the 2 rows of the account erased 4 years ago", n, state)
	}
	if _, state := w.purge("terms_acceptances_purge", 1000, false); state != "55000" {
		t.Fatalf("purge before the legal approval: SQLSTATE %q, want 55000", state)
	}
	for _, bad := range []int{0, 1001} {
		if _, state := w.purge("terms_acceptances_purge", bad, true); state != "22023" {
			t.Errorf("batch size %d: SQLSTATE %q, want 22023", bad, state)
		}
	}
	w.approve("terms_acceptance")
	if n, state := w.purge("terms_acceptances_purge", 1, false); state != "" || n != 1 {
		t.Fatalf("first batch = %d, %q; want 1", n, state)
	}
	if got := queryString(t, w.d, `SELECT notice_version_id::text FROM terms_acceptances WHERE user_id = $1`, gone); got != v2.String() {
		t.Fatalf("oldest first: left %s, want the v2 acceptance", got)
	}
	if n, state := w.purge("terms_acceptances_purge", 1000, false); state != "" || n != 1 {
		t.Fatalf("second batch = %d, %q; want 1", n, state)
	}
	for user, want := range map[uuid.UUID]int{live: 2, gone: 0, recent: 1} {
		if n := w.count(`SELECT count(*) FROM terms_acceptances WHERE user_id = $1`, user); n != want {
			t.Errorf("acceptances of a user after the purge = %d, want %d", n, want)
		}
	}
	// The guard is closed again after the purge (transaction-local setting).
	expectFail(t, owner, []p1aCase{{name: "delete after purge", sql: `DELETE FROM terms_acceptances WHERE user_id = $1`, args: []any{live}, state: "23001"}})

	fn := "public.terms_acceptances_purge(timestamptz, integer, boolean)"
	if got := queryString(t, w.d, `SELECT prosecdef::text || ' ' || proowner::regrole::text FROM pg_proc WHERE oid = $1::regprocedure`, fn); got != "true app_owner" {
		t.Errorf("%s: SECURITY DEFINER and owner = %q, want true app_owner", fn, got)
	}
	for role, want := range map[string]string{"app_rw": "true", "app_ro": "false", "app_talent": "false"} {
		if got := queryString(t, w.d, `SELECT has_function_privilege($1, $2, 'EXECUTE')::text`, role, fn); got != want {
			t.Errorf("EXECUTE on %s for %s = %s, want %s", fn, role, got, want)
		}
	}
}
