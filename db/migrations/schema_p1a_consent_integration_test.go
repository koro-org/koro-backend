//go:build integration

package migrations_test

import (
	"fmt"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/db/migrations"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_ConsentsAppendOnly (02 §4.3, §0.5): the consent ledger takes
// inserts only. app_rw has no UPDATE/DELETE/TRUNCATE grant and the
// koro_forbid_mutation triggers stop even the owner; the rows stay intact.
func TestSchema_ConsentsAppendOnly(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()
	nv := w.notice("commercial_email", "2026.10.02")
	granted, withdrawn := uuid.NewV7(), uuid.NewV7()
	w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, notice_version_id, text_sha256, channel, ip, user_agent)
		VALUES ($1, $2, 'commercial_email', 'granted', $3, $4, 'web', '203.0.113.7', 'test-agent')`, granted, u, nv, sha("Metin"))
	w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, channel)
		VALUES ($1, $2, 'commercial_email', 'withdrawn', 'email_link')`, withdrawn, u) // a withdrawal needs no text proof

	// Layer 1: no grant for app_rw.
	denied := []p1aCase{
		{"app_rw update", `UPDATE consents SET channel = 'admin' WHERE id = $1`, []any{granted}, db.CodeInsufficientPriv, ""},
		{"app_rw update withdrawn row", `UPDATE consents SET action = 'granted' WHERE id = $1`, []any{withdrawn}, db.CodeInsufficientPriv, ""},
		{"app_rw delete", `DELETE FROM consents WHERE id = $1`, []any{granted}, db.CodeInsufficientPriv, ""},
		{"app_rw truncate", `TRUNCATE consents`, nil, db.CodeInsufficientPriv, ""},
	}
	expectFail(t, w.d.Pool, denied)

	// Layer 2: the triggers hold against the table owner too.
	owner := testutil.OwnerConn(t, w.d)
	restricted := []p1aCase{
		{"owner update", `UPDATE consents SET channel = 'admin' WHERE id = $1`, []any{granted}, db.CodeRestrictViolation, ""},
		{"owner update to the same value", `UPDATE consents SET channel = channel WHERE id = $1`, []any{granted}, db.CodeRestrictViolation, ""},
		{"owner delete", `DELETE FROM consents WHERE id = $1`, []any{withdrawn}, db.CodeRestrictViolation, ""},
		// iys_submissions refers to consents, so a plain TRUNCATE consents stops at the FK (0A000).
		{"owner truncate", `TRUNCATE consents, iys_submissions`, nil, db.CodeRestrictViolation, ""},
		{"owner truncate cascade", `TRUNCATE consents CASCADE`, nil, db.CodeRestrictViolation, ""},
	}
	expectFail(t, owner, restricted)

	if n := w.count(`SELECT count(*) FROM consents WHERE user_id = $1`, u); n != 2 {
		t.Fatalf("consents of the user = %d, want 2: the ledger changed", n)
	}
	var triggers []string
	rows, err := w.d.Pool.Query(t.Context(), `SELECT tgname FROM pg_trigger WHERE tgrelid = 'consents'::regclass AND NOT tgisinternal ORDER BY tgname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		triggers = append(triggers, n)
	}
	rows.Close()
	if want := []string{"consents_immutable", "consents_no_truncate"}; !slices.Equal(triggers, want) {
		t.Errorf("triggers on consents = %v, want %v", triggers, want)
	}

	// What the ledger does accept: the shape rules of 02 §4.3.
	other := uuid.NewV7()
	expectFail(t, w.d.Pool, []p1aCase{
		{"granted without notice version", `INSERT INTO consents (user_id, purpose_code, action, text_sha256, channel) VALUES ($1, 'commercial_email', 'granted', $2, 'web')`, []any{u, sha("Metin")}, db.CodeCheckViolation, "consents_proof_chk"},
		{"granted without text digest", `INSERT INTO consents (user_id, purpose_code, action, notice_version_id, channel) VALUES ($1, 'commercial_email', 'granted', $2, 'web')`, []any{u, nv}, db.CodeCheckViolation, "consents_proof_chk"},
		{"registration scope without scope id", `INSERT INTO consents (user_id, purpose_code, action, scope_type, channel) VALUES ($1, 'event_sponsor_share', 'withdrawn', 'registration', 'web')`, []any{u}, db.CodeCheckViolation, "consents_scope_chk"},
		{"scope id without scope type", `INSERT INTO consents (user_id, purpose_code, action, scope_id, channel) VALUES ($1, 'event_sponsor_share', 'withdrawn', $2, 'web')`, []any{u, other}, db.CodeCheckViolation, "consents_scope_chk"},
		{"unknown action", `INSERT INTO consents (user_id, purpose_code, action, channel) VALUES ($1, 'commercial_email', 'revoked', 'web')`, []any{u}, db.CodeCheckViolation, "consents_action_chk"},
		{"unknown channel", `INSERT INTO consents (user_id, purpose_code, action, channel) VALUES ($1, 'commercial_email', 'withdrawn', 'carrier_pigeon')`, []any{u}, db.CodeCheckViolation, "consents_channel_chk"},
		{"unknown scope type", `INSERT INTO consents (user_id, purpose_code, action, scope_type, scope_id, channel) VALUES ($1, 'commercial_email', 'withdrawn', 'event', $2, 'web')`, []any{u, other}, db.CodeCheckViolation, "consents_scope_type_chk"},
		{"text digest of the wrong length", `INSERT INTO consents (user_id, purpose_code, action, notice_version_id, text_sha256, channel) VALUES ($1, 'commercial_email', 'granted', $2, '\x0102', 'web')`, []any{u, nv}, db.CodeCheckViolation, "consents_text_sha256_chk"},
		{"over-long user agent", `INSERT INTO consents (user_id, purpose_code, action, channel, user_agent) VALUES ($1, 'commercial_email', 'withdrawn', 'web', $2)`, []any{u, repeat("a", 401)}, db.CodeCheckViolation, "consents_user_agent_chk"},
		{"unknown purpose", `INSERT INTO consents (user_id, purpose_code, action, channel) VALUES ($1, 'no_such_purpose', 'withdrawn', 'web')`, []any{u}, db.CodeForeignKeyViolation, "consents_purpose_code_fkey"},
		{"unknown user", `INSERT INTO consents (user_id, purpose_code, action, channel) VALUES ($1, 'commercial_email', 'withdrawn', 'web')`, []any{other}, db.CodeForeignKeyViolation, "consents_user_id_fkey"},
		{"unknown notice version", `INSERT INTO consents (user_id, purpose_code, action, notice_version_id, text_sha256, channel) VALUES ($1, 'commercial_email', 'granted', $2, $3, 'web')`, []any{u, other, sha("Metin")}, db.CodeForeignKeyViolation, "consents_notice_version_id_fkey"},
	})
	// A purpose with rows cannot be removed (NO ACTION, 02 §0.1).
	expectFail(t, owner, []p1aCase{
		{"purpose with consents cannot be deleted", `DELETE FROM consent_purposes WHERE code = 'commercial_email'`, nil, db.CodeForeignKeyViolation, ""},
		{"user with consents cannot be deleted", `DELETE FROM users WHERE id = $1`, []any{u}, db.CodeForeignKeyViolation, ""},
	})
}

// TestSchema_ConsentCurrentView (02 §4.3): consent_current has one row per
// (user, purpose, scope): the latest by occurred_at, ties broken by the
// larger id; NULL scopes collapse into one group; the view is read-only.
func TestSchema_ConsentCurrentView(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u1, u2 := w.user(), w.user()
	email := w.notice("commercial_email", "2026.10.02")
	sponsor := w.notice("event_sponsor_share", "2026.10.02")
	discovery := w.notice("discovery_profile", "2026.10.02")
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)
	reg1, reg2 := uuid.NewV7(), uuid.NewV7()

	grant := func(user uuid.UUID, purpose string, notice uuid.UUID, scope *uuid.UUID, at time.Time) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		scopeType := "none"
		if scope != nil {
			scopeType = "registration"
		}
		w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, notice_version_id, text_sha256, scope_type, scope_id, channel, occurred_at)
			VALUES ($1, $2, $3, 'granted', $4, $5, $6, $7, 'web', $8)`, id, user, purpose, notice, sha("Metin"), scopeType, scope, at)
		return id
	}
	withdraw := func(id uuid.UUID, user uuid.UUID, purpose string, scope *uuid.UUID, at time.Time) {
		t.Helper()
		scopeType := "none"
		if scope != nil {
			scopeType = "registration"
		}
		w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, scope_type, scope_id, channel, occurred_at)
			VALUES ($1, $2, $3, 'withdrawn', $4, $5, 'web', $6)`, id, user, purpose, scopeType, scope, at)
	}

	// u1 commercial_email: granted → withdrawn → granted again: granted wins.
	grant(u1, "commercial_email", email, nil, t0)
	withdraw(uuid.NewV7(), u1, "commercial_email", nil, t1)
	grant(u1, "commercial_email", email, nil, t2)
	// u1 event_sponsor_share: per registration. reg1 granted then withdrawn, reg2 granted.
	grant(u1, "event_sponsor_share", sponsor, &reg1, t0)
	withdraw(uuid.NewV7(), u1, "event_sponsor_share", &reg1, t1)
	grant(u1, "event_sponsor_share", sponsor, &reg2, t0)
	// u2 commercial_email: only a withdrawal.
	withdraw(uuid.NewV7(), u2, "commercial_email", nil, t0)
	// u2 discovery_profile: two rows at the same instant. The larger id wins
	// whatever the insertion order: the 'granted' row has the larger id and is
	// inserted first, the 'withdrawn' one second.
	lo, hi := uuid.NewV7(), uuid.NewV7()
	if lo.Compare(hi) >= 0 {
		t.Fatalf("NewV7 is not increasing: %s, %s", lo, hi)
	}
	w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, notice_version_id, text_sha256, channel, occurred_at)
		VALUES ($1, $2, 'discovery_profile', 'granted', $3, $4, 'web', $5)`, hi, u2, discovery, sha("Metin"), t1)
	withdraw(lo, u2, "discovery_profile", nil, t1)

	type key struct {
		user    uuid.UUID
		purpose string
		scope   uuid.UUID // Nil() for NULL
	}
	want := map[key]string{
		{u1, "commercial_email", uuid.Nil()}:  "granted",
		{u1, "event_sponsor_share", reg1}:     "withdrawn",
		{u1, "event_sponsor_share", reg2}:     "granted",
		{u2, "commercial_email", uuid.Nil()}:  "withdrawn",
		{u2, "discovery_profile", uuid.Nil()}: "granted",
	}
	rows, err := w.d.Pool.Query(t.Context(), `SELECT user_id, purpose_code, scope_id, action, occurred_at FROM consent_current`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[key]string{}
	for rows.Next() {
		var (
			k      key
			scope  *uuid.UUID
			action string
			at     time.Time
		)
		if err := rows.Scan(&k.user, &k.purpose, &scope, &action, &at); err != nil {
			t.Fatal(err)
		}
		k.scope = uuid.Nil()
		if scope != nil {
			k.scope = *scope
		}
		if _, dup := got[k]; dup {
			t.Errorf("consent_current has two rows for %v", k)
		}
		got[k] = action
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("consent_current has %d rows, want %d: %v", len(got), len(want), got)
	}
	for k, action := range want {
		if got[k] != action {
			t.Errorf("current consent of %v = %q, want %q", k, got[k], action)
		}
	}

	// A user sees only their own history; the view returns the full row.
	if n := w.count(`SELECT count(*) FROM consent_current WHERE user_id = $1`, u1); n != 3 {
		t.Errorf("consent_current rows of u1 = %d, want 3", n)
	}
	var noticeID *uuid.UUID
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT notice_version_id FROM consent_current WHERE user_id = $1 AND purpose_code = 'commercial_email'`, u1).Scan(&noticeID); err != nil || noticeID == nil || *noticeID != email {
		t.Errorf("current commercial_email notice version = %v, %v; want %s", noticeID, err, email)
	}

	// Read-only for the application.
	expectFail(t, w.d.Pool, []p1aCase{
		{"insert into the view", `INSERT INTO consent_current (user_id, purpose_code, action, channel) VALUES ($1, 'commercial_email', 'withdrawn', 'web')`, []any{u1}, codeObjectState, ""},
		{"update through the view", `UPDATE consent_current SET action = 'granted'`, nil, codeObjectState, ""},
		{"delete through the view", `DELETE FROM consent_current`, nil, codeObjectState, ""},
	})
}

// purposeLine renders one consent_purposes row for comparisons.
func purposeLine(code, basis, channel, scope string, requiresIYS, active bool) string {
	return fmt.Sprintf("%s|%s|%s|%s|%t|%t", code, basis, channel, scope, requiresIYS, active)
}

var seededPurposes = []struct {
	code, basis, channel, scope string
	requiresIYS, active         bool
}{
	{"activity_ranking", "explicit_consent", "", "none", false, false},
	{"commercial_email", "explicit_consent", "email", "none", true, true},
	{"commercial_push", "explicit_consent", "", "none", false, true},
	{"commercial_sms", "explicit_consent", "sms", "none", true, true},
	{"contact_info_share", "explicit_consent", "", "none", false, true},
	{"discovery_profile", "explicit_consent", "", "none", false, true},
	{"event_sponsor_share", "explicit_consent", "", "registration", false, true},
}

// currentPurposes lists consent_purposes in the shape of seededPurposes.
func currentPurposes(t *testing.T, w *p1aWorld) []string {
	t.Helper()
	rows, err := w.d.Pool.Query(t.Context(), `SELECT code, legal_basis, coalesce(iys_channel, ''), scope_type, requires_iys, active
		FROM consent_purposes ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code, basis, channel, scope string
		var iys, active bool
		if err := rows.Scan(&code, &basis, &channel, &scope, &iys, &active); err != nil {
			t.Fatal(err)
		}
		out = append(out, purposeLine(code, basis, channel, scope, iys, active))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSchema_SeedConsentPurposes_Idempotent (02 §4.1, 000011): the seven v1
// purposes exist after the migrations, with the documented İYS channel,
// scope and activity; running the seed again changes nothing and never
// overwrites an edit an admin made (ON CONFLICT DO NOTHING); it restores a
// row that is missing.
func TestSchema_SeedConsentPurposes_Idempotent(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	var want []string
	for _, p := range seededPurposes {
		want = append(want, purposeLine(p.code, p.basis, p.channel, p.scope, p.requiresIYS, p.active))
	}
	if got := currentPurposes(t, w); !slices.Equal(got, want) {
		t.Fatalf("seeded purposes:\n got %v\nwant %v", got, want)
	}

	seed, err := migrations.FS.ReadFile("000011_seed_consent_purposes.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	owner := testutil.OwnerConn(t, w.d)

	// An admin edit (consent.purpose.manage) survives a re-run.
	w.exec(`UPDATE consent_purposes SET description_tr = 'Düzenlendi', active = false WHERE code = 'commercial_email'`)
	for i := range 2 {
		if _, err := owner.Exec(t.Context(), string(seed)); err != nil {
			t.Fatalf("seed run %d: %v", i+1, err)
		}
	}
	var description string
	var active bool
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT description_tr, active FROM consent_purposes WHERE code = 'commercial_email'`).Scan(&description, &active); err != nil {
		t.Fatal(err)
	}
	if description != "Düzenlendi" || active {
		t.Errorf("seed overwrote an edited purpose: %q active=%v", description, active)
	}
	w.exec(`UPDATE consent_purposes SET active = true WHERE code = 'commercial_email'`)
	if n := w.count(`SELECT count(*) FROM consent_purposes`); n != len(seededPurposes) {
		t.Errorf("purposes after re-running the seed = %d, want %d", n, len(seededPurposes))
	}

	// A missing row comes back; the others are left alone.
	if _, err := owner.Exec(t.Context(), `DELETE FROM consent_purposes WHERE code = 'commercial_push'`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), string(seed)); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM consent_purposes WHERE code = 'commercial_push' AND NOT requires_iys AND iys_channel IS NULL`); n != 1 {
		t.Errorf("deleted purpose was not restored (%d)", n)
	}
	if n := w.count(`SELECT count(*) FROM consent_purposes`); n != len(seededPurposes) {
		t.Errorf("purposes after the restore = %d, want %d", n, len(seededPurposes))
	}
}

// TestSchema_ConsentPurposeConstraints covers the shape rules of
// consent_purposes (02 §4.1) and the grants of the application.
func TestSchema_ConsentPurposeConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	const base = `INSERT INTO consent_purposes (code, description_tr, legal_basis`
	expectFail(t, w.d.Pool, []p1aCase{
		{"code with a space and capitals", base + `) VALUES ('Bad Code', 'x', 'explicit_consent')`, nil, db.CodeCheckViolation, "consent_purposes_code_chk"},
		{"code too short", base + `) VALUES ('ab', 'x', 'explicit_consent')`, nil, db.CodeCheckViolation, "consent_purposes_code_chk"},
		{"code starting with a digit", base + `) VALUES ('1abc', 'x', 'explicit_consent')`, nil, db.CodeCheckViolation, "consent_purposes_code_chk"},
		{"unknown legal basis", base + `) VALUES ('new_purpose', 'x', 'whim')`, nil, db.CodeCheckViolation, "consent_purposes_legal_basis_chk"},
		{"İYS required without a channel", base + `, requires_iys) VALUES ('iys_no_channel', 'x', 'explicit_consent', true)`, nil, db.CodeCheckViolation, "consent_purposes_iys_chk"},
		{"İYS channel without the requirement", base + `, iys_channel) VALUES ('channel_no_iys', 'x', 'explicit_consent', 'email')`, nil, db.CodeCheckViolation, "consent_purposes_iys_chk"},
		{"unknown İYS channel", base + `, requires_iys, iys_channel) VALUES ('iys_fax', 'x', 'explicit_consent', true, 'fax')`, nil, db.CodeCheckViolation, "consent_purposes_iys_channel_chk"},
		{"unknown scope type", base + `, scope_type) VALUES ('scoped_wrong', 'x', 'explicit_consent', 'event')`, nil, db.CodeCheckViolation, "consent_purposes_scope_type_chk"},
		{"duplicate code", base + `) VALUES ('commercial_email', 'x', 'explicit_consent')`, nil, db.CodeUniqueViolation, "consent_purposes_pkey"},
		{"missing description", `INSERT INTO consent_purposes (code, legal_basis) VALUES ('no_description', 'explicit_consent')`, nil, codeNotNull, ""},
		{"app_rw cannot delete a purpose", `DELETE FROM consent_purposes WHERE code = 'activity_ranking'`, nil, db.CodeInsufficientPriv, ""},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"new purpose with an İYS channel", base + `, requires_iys, iys_channel) VALUES ('commercial_call', 'x', 'explicit_consent', true, 'call')`, nil, "", ""},
		{"admin edit", `UPDATE consent_purposes SET active = false WHERE code = 'commercial_call'`, nil, "", ""},
	})
}

// TestSchema_NoticeVersionConstraints covers notice_versions (02 §4.2): the
// kind/purpose pairing, the version format, the unique version and the
// unique current version per key, and notice_versions_guard (a published
// row is frozen except for being retired once, and cannot be deleted).
func TestSchema_NoticeVersionConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	admin := w.user()
	sha32 := sha("body")
	const cols = `INSERT INTO notice_versions (kind, purpose_code, audience, locale, version, title, body_md, body_sha256, published_at, retired_at, created_by) VALUES `

	expectFail(t, w.d.Pool, []p1aCase{
		{"consent_text without a purpose", cols + `('consent_text', NULL, 'all', 'tr-TR', '2026.10.02', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_purpose_chk"},
		{"privacy_notice with a purpose", cols + `('privacy_notice', 'commercial_email', 'all', 'tr-TR', '2026.10.02', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_purpose_chk"},
		{"unknown purpose", cols + `('consent_text', 'no_such_purpose', 'all', 'tr-TR', '2026.10.02', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeForeignKeyViolation, "notice_versions_purpose_code_fkey"},
		{"retired but never published", cols + `('privacy_notice', NULL, 'all', 'tr-TR', '2026.10.02', 't', 'b', $1, NULL, now(), NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_retired_chk"},
		{"version with dashes", cols + `('privacy_notice', NULL, 'all', 'tr-TR', '2026-10-02', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_version_chk"},
		{"version without a day", cols + `('privacy_notice', NULL, 'all', 'tr-TR', '2026.10', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_version_chk"},
		{"body digest of the wrong length", cols + `('privacy_notice', NULL, 'all', 'tr-TR', '2026.10.02', 't', 'b', '\x0102', NULL, NULL, NULL)`, nil, db.CodeCheckViolation, "notice_versions_body_sha256_chk"},
		{"title too long", cols + `('privacy_notice', NULL, 'all', 'tr-TR', '2026.10.02', '` + repeat("t", 201) + `', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_title_chk"},
		{"unknown kind", cols + `('press_release', NULL, 'all', 'tr-TR', '2026.10.02', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_kind_chk"},
		{"unknown audience", cols + `('privacy_notice', NULL, 'robots', 'tr-TR', '2026.10.02', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_audience_chk"},
		{"unknown locale", cols + `('privacy_notice', NULL, 'all', 'de-DE', '2026.10.02', 't', 'b', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeCheckViolation, "notice_versions_locale_chk"},
		{"unknown creator", cols + `('privacy_notice', NULL, 'all', 'tr-TR', '2026.10.02', 't', 'b', $1, NULL, NULL, $2)`, []any{sha32, uuid.NewV7()}, db.CodeForeignKeyViolation, "notice_versions_created_by_fkey"},
	})

	// Two privacy notices of one key and version collide although purpose_code
	// is NULL (the index uses coalesce).
	expectOK(t, w.d.Pool, []p1aCase{
		{"published privacy notice", cols + `('privacy_notice', NULL, 'student', 'tr-TR', '2026.10.02', 'Aydınlatma', 'v1', $1, now(), NULL, $2)`, []any{sha32, admin}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"same key and version again", cols + `('privacy_notice', NULL, 'student', 'tr-TR', '2026.10.02', 'Aydınlatma', 'v1 again', $1, NULL, NULL, NULL)`, []any{sha32}, db.CodeUniqueViolation, "notice_versions_version_uq"},
		{"second current version of the key", cols + `('privacy_notice', NULL, 'student', 'tr-TR', '2026.11.01', 'Aydınlatma', 'v2', $1, now(), NULL, NULL)`, []any{sha32}, db.CodeUniqueViolation, "notice_versions_current_uq"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"same version for another audience", cols + `('privacy_notice', NULL, 'company', 'tr-TR', '2026.10.02', 'Aydınlatma', 'v1', $1, now(), NULL, NULL)`, []any{sha32}, "", ""},
		{"same version in another locale", cols + `('privacy_notice', NULL, 'student', 'en-US', '2026.10.02', 'Notice', 'v1', $1, now(), NULL, NULL)`, []any{sha32}, "", ""},
		{"a draft of the next version does not count as current", cols + `('privacy_notice', NULL, 'student', 'tr-TR', '2026.11.01', 'Aydınlatma', 'v2 draft', $1, NULL, NULL, NULL)`, []any{sha32}, "", ""},
	})

	// The guard: a published row is frozen, only retiring it (once) is allowed.
	var current uuid.UUID
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT id FROM notice_versions WHERE kind = 'privacy_notice' AND audience = 'student' AND locale = 'tr-TR' AND version = '2026.10.02'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	frozen := []p1aCase{
		{"edit the body of a published version", `UPDATE notice_versions SET body_md = 'changed' WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
		{"edit the title", `UPDATE notice_versions SET title = 'changed' WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
		{"edit the digest", `UPDATE notice_versions SET body_sha256 = $2 WHERE id = $1`, []any{current, sha("other")}, db.CodeRestrictViolation, ""},
		{"unpublish", `UPDATE notice_versions SET published_at = NULL WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
		{"retire and edit at once", `UPDATE notice_versions SET retired_at = now(), body_md = 'changed' WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
		{"owner deletes a published version", `DELETE FROM notice_versions WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
	}
	expectFail(t, owner, frozen)
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw cannot delete a published version", `DELETE FROM notice_versions WHERE id = $1`, []any{current}, db.CodeInsufficientPriv, ""},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"retire the current version", `UPDATE notice_versions SET retired_at = now() WHERE id = $1`, []any{current}, "", ""},
		{"publish the next version afterwards", `UPDATE notice_versions SET published_at = now(), body_md = 'v2 final'
			WHERE kind = 'privacy_notice' AND audience = 'student' AND locale = 'tr-TR' AND version = '2026.11.01'`, nil, "", ""},
	})
	expectFail(t, owner, []p1aCase{
		{"un-retire", `UPDATE notice_versions SET retired_at = NULL WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
		{"retire again with another time", `UPDATE notice_versions SET retired_at = now() + interval '1 day' WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
		{"a retired version cannot be deleted", `DELETE FROM notice_versions WHERE id = $1`, []any{current}, db.CodeRestrictViolation, ""},
	})
	// Drafts stay editable and (for the owner) deletable.
	expectOK(t, w.d.Pool, []p1aCase{
		{"new draft", cols + `('privacy_notice', NULL, 'student', 'tr-TR', '2027.01.01', 'Taslak', 'draft', $1, NULL, NULL, NULL)`, []any{sha32}, "", ""},
	})
	expectOK(t, owner, []p1aCase{
		{"edit a draft", `UPDATE notice_versions SET body_md = 'draft edited' WHERE published_at IS NULL`, nil, "", ""},
		{"delete a draft", `DELETE FROM notice_versions WHERE published_at IS NULL`, nil, "", ""},
	})
	if n := w.count(`SELECT count(*) FROM notice_versions WHERE published_at IS NULL`); n != 0 {
		t.Errorf("drafts left = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM notice_versions WHERE id = $1`, current); n != 1 {
		t.Errorf("the retired version disappeared")
	}
}

// TestSchema_IYSSubmissionConstraints covers iys_submissions (02 §4.4).
func TestSchema_IYSSubmissionConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()
	notice := w.notice("commercial_email", "2026.10.02")
	newConsent := func() uuid.UUID {
		id := uuid.NewV7()
		w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, notice_version_id, text_sha256, channel)
			VALUES ($1, $2, 'commercial_email', 'granted', $3, $4, 'web')`, id, u, notice, sha("Metin"))
		return id
	}
	c1, c2 := newConsent(), newConsent()
	w.exec(`INSERT INTO iys_submissions (consent_id, channel) VALUES ($1, 'email')`, c1)
	expectFail(t, w.d.Pool, []p1aCase{
		{"second submission for one consent", `INSERT INTO iys_submissions (consent_id, channel) VALUES ($1, 'email')`, []any{c1}, db.CodeUniqueViolation, "iys_submissions_consent_id_uq"},
		{"unknown consent", `INSERT INTO iys_submissions (consent_id, channel) VALUES ($1, 'email')`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "iys_submissions_consent_id_fkey"},
		{"sent without sent_at", `INSERT INTO iys_submissions (consent_id, channel, status) VALUES ($1, 'email', 'sent')`, []any{c2}, db.CodeCheckViolation, "iys_submissions_sent_chk"},
		{"unknown channel", `INSERT INTO iys_submissions (consent_id, channel) VALUES ($1, 'fax')`, []any{c2}, db.CodeCheckViolation, "iys_submissions_channel_chk"},
		{"unknown status", `INSERT INTO iys_submissions (consent_id, channel, status) VALUES ($1, 'email', 'lost')`, []any{c2}, db.CodeCheckViolation, "iys_submissions_status_chk"},
		{"negative attempts", `INSERT INTO iys_submissions (consent_id, channel, attempts) VALUES ($1, 'email', -1)`, []any{c2}, db.CodeCheckViolation, "iys_submissions_attempts_chk"},
		{"over-long error", `INSERT INTO iys_submissions (consent_id, channel, last_error) VALUES ($1, 'email', $2)`, []any{c2, repeat("e", 2001)}, db.CodeCheckViolation, "iys_submissions_last_error_chk"},
	})
	// The state machine writes pending → sent; updated_at moves with it. A
	// submission is deleted together with its consent (hard delete).
	w.exec(`UPDATE iys_submissions SET status = 'sent', sent_at = now(), attempts = 1, iys_transaction_id = 'tx-1' WHERE consent_id = $1`, c1)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM iys_submissions WHERE consent_id = $1`, c1); got != "true" {
		t.Errorf("updated_at was not stamped on update")
	}
	w.exec(`DELETE FROM iys_submissions WHERE consent_id = $1`, c1)
}
