//go:build integration

package migrations_test

import (
	"regexp"
	"slices"
	"testing"
	"uuid"

	"koro/db/migrations"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_FormVersionsImmutable (02 §11.4, §0.5): a published form version
// is never edited. app_rw has no UPDATE/DELETE/TRUNCATE grant and the
// koro_forbid_mutation triggers stop even the owner; the form that owns a
// version cannot be deleted either.
func TestSchema_FormVersionsImmutable(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, user := w.org("club"), w.user()
	ev := w.event(org, user, nil)
	form := w.form(ev, org)
	v1 := w.formVersion(form, ev, user, nil)
	v2 := w.formVersion(form, ev, user, map[string]string{"version_no": "2"})
	w.exec(`UPDATE event_forms SET current_version_id = $2 WHERE id = $1`, form, v2) // the pointer moves, the versions do not

	// Layer 1: no grant for app_rw.
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw update", `UPDATE event_form_versions SET field_count = 3 WHERE id = $1`, []any{v1}, db.CodeInsufficientPriv, ""},
		{"app_rw update of the schema", `UPDATE event_form_versions SET schema = '{"schema_version":1,"fields":[]}' WHERE id = $1`, []any{v1}, db.CodeInsufficientPriv, ""},
		{"app_rw delete", `DELETE FROM event_form_versions WHERE id = $1`, []any{v1}, db.CodeInsufficientPriv, ""},
		{"app_rw truncate", `TRUNCATE event_form_versions`, nil, db.CodeInsufficientPriv, ""},
	})
	// Layer 2: the triggers hold against the table owner too.
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, owner, []p1aCase{
		{"owner update", `UPDATE event_form_versions SET field_count = 3 WHERE id = $1`, []any{v1}, db.CodeRestrictViolation, ""},
		{"owner update to the same value", `UPDATE event_form_versions SET field_count = field_count WHERE id = $1`, []any{v2}, db.CodeRestrictViolation, ""},
		{"owner delete", `DELETE FROM event_form_versions WHERE id = $1`, []any{v1}, db.CodeRestrictViolation, ""},
		// Every table that refers to the versions (directly or through registrations) is named, else the FK (0A000) stops first.
		{"owner truncate", `TRUNCATE event_form_versions, event_forms, event_registrations, registration_tickets, event_checkins`, nil, db.CodeRestrictViolation, ""},
		{"owner truncate cascade", `TRUNCATE event_form_versions CASCADE`, nil, db.CodeRestrictViolation, ""},
		{"owner deletes the form of a version", `DELETE FROM event_forms WHERE id = $1`, []any{form}, db.CodeForeignKeyViolation, "event_form_versions_form_id_fkey"},
	})
	if n := w.count(`SELECT count(*) FROM event_form_versions WHERE form_id = $1`, form); n != 2 {
		t.Fatalf("versions = %d, want 2: the history changed", n)
	}
	if got := queryStrings(t, w, `SELECT tgname FROM pg_trigger WHERE tgrelid = 'event_form_versions'::regclass AND NOT tgisinternal ORDER BY tgname`); !slices.Equal(got, []string{"event_form_versions_immutable", "event_form_versions_no_truncate"}) {
		t.Errorf("triggers on event_form_versions = %v", got)
	}
	// A registration keeps pointing at the version it was made against.
	reg := w.registration(ev, org, w.student(), map[string]string{"form_version_id": "'" + v1.String() + "'"})
	if got := queryString(t, w.d, `SELECT form_version_id::text FROM event_registrations WHERE id = $1`, reg); got != v1.String() {
		t.Errorf("registration form version = %s, want %s", got, v1)
	}
}

// TestSchema_FormVersionConstraints (02 §11.4): shape of a version, the
// reasoned override of the KVKK lint, and the lint rules that 02 states in
// prose and the DB enforces: a version with a 'blocked' field cannot exist
// (no override) and has_sensitive_fields cannot hide a 'warn_special' field.
func TestSchema_FormVersionConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, user := w.org("club"), w.user()
	ev := w.event(org, user, nil)
	form := w.form(ev, org)
	// $1 = form, $2 = event, $3 = publisher, $4 = version id, $5 = digest.
	fv := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("event_form_versions", formVersionDefaults, over), args, state, constraint}
	}
	std := func() []any { return []any{form, ev, user, uuid.NewV7(), bytes32("v-" + w.n())} }
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return fv(name, over, std(), db.CodeCheckViolation, constraint)
	}
	report := func(blocked, warn, review string) string {
		return `'{"blocked":` + blocked + `,"warn_special":` + warn + `,"review_identifier":` + review + `}'::jsonb`
	}
	override := map[string]string{"lint_override_by": "$3", "lint_override_reason": lit("Diyet bilgisi güvenlik için gerekli")}
	badDigest := std()
	badDigest[4] = []byte{1, 2, 3}
	other := uuid.NewV7()
	expectFail(t, w.d.Pool, []p1aCase{
		chk("version number 0", map[string]string{"version_no": "0"}, "event_form_versions_version_no_chk"),
		fv("a digest of the wrong length", nil, badDigest, db.CodeCheckViolation, "event_form_versions_schema_sha256_chk"),
		chk("21 fields", map[string]string{"field_count": "21"}, "event_form_versions_field_count_chk"),
		chk("negative field count", map[string]string{"field_count": "-1"}, "event_form_versions_field_count_chk"),
		chk("override reason of 9 characters", cols(map[string]string{"lint_override_by": "$3", "lint_override_reason": lit("too short")}), "event_form_versions_lint_override_reason_chk"),
		chk("override reason of 501 characters", cols(map[string]string{"lint_override_by": "$3", "lint_override_reason": lit(repeat("a", 501))}), "event_form_versions_lint_override_reason_chk"),
		chk("an override without a reason", map[string]string{"lint_override_by": "$3"}, "event_form_versions_override_chk"),
		chk("a reason without an overrider", map[string]string{"lint_override_reason": lit("Diyet bilgisi güvenlik için gerekli")}, "event_form_versions_override_chk"),
		// warn_special: only with a reasoned override, and the flag must say so.
		chk("a sensitive field without an override", map[string]string{"has_sensitive_fields": "true", "lint_report": report("[]", `["diyet"]`, "[]")}, "event_form_versions_sensitive_override_chk"),
		chk("a sensitive field hidden by the flag", cols(override, map[string]string{"lint_report": report("[]", `["diyet"]`, "[]")}), "event_form_versions_sensitive_flag_chk"),
		chk("a sensitive flag without a sensitive field", cols(override, map[string]string{"has_sensitive_fields": "true"}), "event_form_versions_sensitive_flag_chk"),
		// blocked: never, with or without an override.
		chk("a blocked field", map[string]string{"lint_report": report(`["tckn"]`, "[]", "[]")}, "event_form_versions_blocked_chk"),
		chk("a blocked field with an override", cols(override, map[string]string{"lint_report": report(`["tckn"]`, "[]", "[]")}), "event_form_versions_blocked_chk"),
		chk("a blocked and a sensitive field with an override", cols(override, map[string]string{"has_sensitive_fields": "true", "lint_report": report(`["kart no"]`, `["diyet"]`, "[]")}), "event_form_versions_blocked_chk"),
		// the shape of the report
		chk("a report that is an array", map[string]string{"lint_report": `'[]'::jsonb`}, "event_form_versions_lint_report_chk"),
		chk("a report without review_identifier", map[string]string{"lint_report": `'{"blocked":[],"warn_special":[]}'::jsonb`}, "event_form_versions_lint_report_chk"),
		chk("a report with a non-array list", map[string]string{"lint_report": `'{"blocked":[],"warn_special":[],"review_identifier":"none"}'::jsonb`}, "event_form_versions_lint_report_chk"),
		chk("an empty report", map[string]string{"lint_report": `'{}'::jsonb`}, "event_form_versions_lint_report_chk"),
		fv("unknown form", nil, []any{other, ev, user, uuid.NewV7(), bytes32("x")}, db.CodeForeignKeyViolation, "event_form_versions_form_id_fkey"),
		fv("unknown event", nil, []any{form, other, user, uuid.NewV7(), bytes32("x")}, db.CodeForeignKeyViolation, "event_form_versions_event_id_fkey"),
		fv("unknown publisher", nil, []any{form, ev, other, uuid.NewV7(), bytes32("x")}, db.CodeForeignKeyViolation, "event_form_versions_published_by_fkey"),
		fv("unknown overrider", map[string]string{"lint_override_by": lit(other.String()), "lint_override_reason": lit("Diyet bilgisi güvenlik için gerekli")}, std(), db.CodeForeignKeyViolation, "event_form_versions_lint_override_by_fkey"),
	})

	expectOK(t, w.d.Pool, []p1aCase{
		fv("a clean version", nil, std(), "", ""),
		fv("a version with review_identifier findings", map[string]string{"version_no": "2", "lint_report": report("[]", "[]", `["pasaport"]`)}, std(), "", ""),
		fv("a sensitive version with a reasoned override", cols(override, map[string]string{"version_no": "3", "has_sensitive_fields": "true", "lint_report": report("[]", `["diyet"]`, "[]")}), std(), "", ""),
		fv("a clean version published with a (needless) override", cols(override, map[string]string{"version_no": "4"}), std(), "", ""),
		fv("20 fields", map[string]string{"version_no": "5", "field_count": "20"}, std(), "", ""),
	})
	expectFail(t, w.d.Pool, []p1aCase{
		fv("the same version number twice", nil, std(), db.CodeUniqueViolation, "event_form_versions_form_version_uq"),
	})
}

// TestSchema_SeedFormLintRules (02 §11.5): the seed rules (000060) have the
// classes and shapes the lint needs, their patterns are valid RE2 (Go regexp,
// the engine of the service), and on koro_norm-ed labels they classify the
// field titles of a real form as intended.
func TestSchema_SeedFormLintRules(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	rows, err := w.d.Pool.Query(t.Context(), `SELECT class, pattern, description_tr, active FROM form_lint_rules ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type rule struct {
		class, pattern, desc string
		active               bool
		re                   *regexp.Regexp
	}
	var rules []rule
	for rows.Next() {
		var r rule
		if err := rows.Scan(&r.class, &r.pattern, &r.desc, &r.active); err != nil {
			t.Fatal(err)
		}
		re, err := regexp.Compile(r.pattern)
		if err != nil {
			t.Errorf("seed pattern %q is not valid RE2: %v", r.pattern, err)
			continue
		}
		r.re = re
		rules = append(rules, r)
	}
	rows.Close()
	perClass := map[string]int{}
	for _, r := range rules {
		perClass[r.class]++
		if !r.active || r.desc == "" {
			t.Errorf("seed rule %q must be active and described", r.pattern)
		}
	}
	if perClass["blocked"] != 10 || perClass["warn_special"] != 5 || perClass["review_identifier"] != 4 || len(rules) != 19 {
		t.Errorf("seed rules per class = %v (total %d), want blocked 10, warn_special 5, review_identifier 4", perClass, len(rules))
	}

	// What the lint will see is koro_norm(label).
	classify := func(label string) []string {
		norm := queryString(t, w.d, `SELECT koro_norm($1)`, label)
		found := map[string]bool{}
		for _, r := range rules {
			if r.re.MatchString(norm) {
				found[r.class] = true
			}
		}
		out := make([]string, 0, len(found))
		for c := range found {
			out = append(out, c)
		}
		slices.Sort(out)
		return out
	}
	blocked, warn, review := []string{"blocked"}, []string{"warn_special"}, []string{"review_identifier"}
	for label, want := range map[string][]string{
		"T.C. Kimlik Numaranız":                blocked,
		"TCKN":                                 blocked,
		"Kimlik No":                            blocked,
		"Kredi kartı numarası":                 blocked,
		"CVV":                                  blocked,
		"Parolanız":                            blocked,
		"Şifre":                                blocked,
		"IBAN":                                 blocked,
		"Sabıka kaydınız var mı?":              blocked,
		"Siyasi görüşünüz":                     blocked,
		"Parti üyeliğiniz":                     blocked,
		"Etnik kökeniniz":                      blocked,
		"Irkınız":                              blocked,
		"Cinsel yönelim":                       blocked,
		"Sendika üyeliği":                      blocked,
		"Biyometrik veri":                      blocked,
		"Alerjiniz var mı?":                    warn,
		"Sağlık durumunuz":                     warn,
		"Kullandığınız ilaçlar":                warn,
		"Kan grubunuz":                         warn,
		"Engellilik durumunuz":                 warn,
		"Erişilebilirlik ihtiyacınız":          warn,
		"Diyet veya yemek tercihiniz":          warn,
		"Vegan / vejetaryen misiniz?":          warn,
		"Dininiz":                              warn,
		"Dini inancınız":                       warn,
		"Pasaport numarası":                    review,
		"Ehliyet no":                           review,
		"Vergi numarası":                       review,
		"SGK sicil no":                         review,
		"Ad Soyad":                             nil,
		"E-posta adresiniz":                    nil,
		"Telefon numaranız":                    nil,
		"Bölümünüz":                            nil,
		"Sınıfınız":                            nil,
		"Öğrenci numaranız":                    nil,
		"Tişört bedeniniz":                     nil,
		"Kaç kişiyle katılacaksınız?":          nil,
		"Dinleyici olarak mı katılıyorsunuz?":  nil,
		"Gelecek hedefleriniz":                 nil,
		"Hangi konuları merak ediyorsunuz?":    nil,
		"Irmak Sokak, Işık Mahallesi":          nil,
		"Katılım nedeniniz":                    nil,
		"Kimlik kartınızı yanınızda getirin":   nil,
		"Bilgisayar Mühendisliği öğrencisiyim": nil,
	} {
		if got := classify(label); !slices.Equal(got, want) {
			t.Errorf("lint classes of %q = %v, want %v", label, got, want)
		}
	}
}

// TestSchema_SeedFormLintRules_Idempotent: running the seed again changes
// nothing a DPO did (a deactivated rule stays off, an added rule stays) and
// restores a seed row that is gone.
func TestSchema_SeedFormLintRules_Idempotent(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	seed, err := migrations.FS.ReadFile("000060_seed_form_lint_rules.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	const first = "01990000-0000-7000-8000-000000000001"
	w.exec(`UPDATE form_lint_rules SET active = false, pattern = '\bnever\b' WHERE id = $1`, first) // a DPO edit
	w.exec(`INSERT INTO form_lint_rules (class, pattern, description_tr) VALUES ('warn_special', '\bgluten\b', 'Eklenen kural')`)
	if _, err := owner.Exec(t.Context(), `DELETE FROM form_lint_rules WHERE id = '01990000-0000-7000-8000-000000000019'`); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM form_lint_rules`); n != 19 {
		t.Fatalf("rules before the re-run = %d, want 19 (18 seeds + 1 added)", n)
	}
	if _, err := owner.Exec(t.Context(), string(seed)); err != nil {
		t.Fatalf("re-running the seed: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM form_lint_rules`); n != 20 {
		t.Errorf("rules after the re-run = %d, want 20", n)
	}
	if got := queryString(t, w.d, `SELECT active::text || pattern FROM form_lint_rules WHERE id = $1`, first); got != `false\bnever\b` {
		t.Errorf("the edited rule was overwritten: %s", got)
	}
	if n := w.count(`SELECT count(*) FROM form_lint_rules WHERE id = '01990000-0000-7000-8000-000000000019'`); n != 1 {
		t.Errorf("the deleted seed row was not restored")
	}
}

// TestSchema_FormLintRuleConstraints (02 §11.5): rules are deactivated, never
// deleted; an empty pattern (which would match every form) is refused.
func TestSchema_FormLintRuleConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	user := w.user()
	rule := func(name, class, pattern string, desc any, updatedBy any, state, constraint string) p1aCase {
		return p1aCase{name, `INSERT INTO form_lint_rules (class, pattern, description_tr, updated_by) VALUES ($1, $2, $3, $4)`, []any{class, pattern, desc, updatedBy}, state, constraint}
	}
	expectFail(t, w.d.Pool, []p1aCase{
		rule("unknown class", "info", `\bx\b`, "x", nil, db.CodeCheckViolation, "form_lint_rules_class_chk"),
		rule("an empty pattern", "blocked", "", "x", nil, db.CodeCheckViolation, "form_lint_rules_pattern_chk"),
		rule("a pattern of 501 characters", "blocked", repeat("a", 501), "x", nil, db.CodeCheckViolation, "form_lint_rules_pattern_chk"),
		rule("no description", "blocked", `\bx\b`, nil, nil, codeNotNull, ""),
		rule("unknown editor", "blocked", `\bx\b`, "x", uuid.NewV7(), db.CodeForeignKeyViolation, "form_lint_rules_updated_by_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		rule("a pattern of 500 characters", "warn_special", repeat("a", 500), "x", user, "", ""),
	})
	w.exec(`UPDATE form_lint_rules SET active = false, updated_by = $1 WHERE class = 'blocked'`, user)
	if n := w.count(`SELECT count(*) FROM form_lint_rules WHERE active AND class = 'blocked'`); n != 0 {
		t.Errorf("active blocked rules = %d after deactivating them", n)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw deletes a rule", `DELETE FROM form_lint_rules WHERE class = 'blocked'`, nil, db.CodeInsufficientPriv, ""},
	})
}
