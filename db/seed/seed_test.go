package seed_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/db/seed"
	"koro/internal/platform/crypto"
	"koro/internal/platform/iban"
	"koro/internal/platform/testutil/fake"
)

var now = time.Date(2026, time.October, 5, 9, 30, 0, 0, time.UTC)

// reference is a synthetic stand-in for `koro seed reference`.
func reference() seed.Reference {
	ref := seed.Reference{PlatformOrgID: uuid.MustParse("0192f5c4-0000-7000-8000-00000000000f")}
	for i, name := range []string{"Işıkkent Üniversitesi", "Çamlıyamaç Üniversitesi", "Göktaşı Üniversitesi"} {
		u := seed.University{ID: uuid.NewV7(), OrgID: uuid.NewV7(), YokCode: "TEST-" + string(rune('A'+i)), Name: name,
			ShortName: "U" + string(rune('A'+i)), City: "Ankara"}
		for _, d := range []string{"Bilgisayar Mühendisliği", "İktisat", "Psikoloji"} {
			u.Departments = append(u.Departments, seed.Department{ID: uuid.NewV7(), Name: d, DegreeLevel: "bachelor", DurationYears: 4})
		}
		ref.Universities = append(ref.Universities, u)
	}
	return ref
}

func tickets(t *testing.T) *crypto.Deriver {
	t.Helper()
	d, err := crypto.NewDeriver([]crypto.Key{{ID: "t1", Secret: []byte("seed-test-ticket-key-seed-test-ticket-key")}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func build(t *testing.T, ref seed.Reference, at time.Time) *seed.Plan {
	t.Helper()
	p, err := seed.Build(ref, seed.Options{Now: at, Tickets: tickets(t), PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// rows returns the planned rows of table as column → value maps.
func rows(p *seed.Plan, table string) []map[string]any {
	var out []map[string]any
	for _, r := range p.Rows {
		if r.Table != table {
			continue
		}
		m := map[string]any{}
		for i, c := range r.Cols {
			m[c] = r.Vals[i]
		}
		out = append(out, m)
	}
	return out
}

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@([A-Za-z0-9.\-]+\.[A-Za-z]{2,})`)
	// phoneRe: an international number or a Turkish mobile number with or
	// without separators (dates and UUIDs use '-', so they never match).
	phoneRe = regexp.MustCompile(`\+\d[\d ]{8,}\d|\b0?5\d{2} ?\d{3} ?\d{2} ?\d{2}\b`)
	fakePhn = regexp.MustCompile(`^\+90 000 000 00 \d\d$`)
	ibanRe  = regexp.MustCompile(`TR[0-9]{24}`)
	tcknRe  = regexp.MustCompile(`\b[1-9]\d{10}\b`)
)

// allowedDomains are the e-mail domains synthetic data may use (05 §13.7).
var allowedDomains = map[string]bool{"example.test": true, "koro.test": true}

// TestSeedDemo_NoRealPII (05 §13.7, BE-033): every text of the demo plan
// is synthetic: e-mail addresses only @example.test (no real domain, no
// .edu.tr address: the students are verified by document), phone numbers
// only in the invalid +90 000 000 00 NN pattern, no national ID other than
// the documented test vectors, IBANs only checksum-valid nonexistent bank
// codes 99999/00000. The files of db/seed are scanned as well
// (05 §13.7 TestSyntheticOnly).
func TestSeedDemo_NoRealPII(t *testing.T) {
	t.Parallel()
	p := build(t, reference(), now)
	vectors := map[string]bool{}
	for _, v := range fake.TCKNVectors {
		vectors[v.Input] = true
	}
	check := func(where, s string) {
		for _, m := range emailRe.FindAllStringSubmatch(s, -1) {
			if !allowedDomains[strings.ToLower(m[1])] {
				t.Errorf("%s: real-looking e-mail domain %q", where, m[1])
			}
		}
		for _, m := range phoneRe.FindAllString(s, -1) {
			if !fakePhn.MatchString(m) {
				t.Errorf("%s: phone-like value %q outside the invalid pattern", where, m)
			}
		}
		for _, m := range ibanRe.FindAllString(s, -1) {
			if !iban.Valid(m) || (m[4:9] != "99999" && m[4:9] != "00000") {
				t.Errorf("%s: IBAN outside the synthetic vector: %q", where, m)
			}
		}
		for _, m := range tcknRe.FindAllString(s, -1) {
			if !vectors[m] {
				t.Errorf("%s: national-ID-like value %q", where, m)
			}
		}
		if strings.Contains(strings.ToLower(s), ".edu.tr") {
			t.Errorf("%s: an .edu.tr value %q", where, s)
		}
	}
	strs := p.Strings()
	if len(strs) < 1000 {
		t.Fatalf("only %d strings scanned", len(strs))
	}
	for _, s := range strs {
		check("plan", s)
	}
	emails := 0
	for _, u := range rows(p, "users") {
		e := u["email"].(string)
		if !strings.HasSuffix(e, "@"+fake.EmailDomain) {
			t.Errorf("user e-mail %q", e)
		}
		emails++
	}
	if emails < seed.StudentCount {
		t.Fatalf("%d users", emails)
	}
	for _, v := range rows(p, "edu_verifications") {
		if v["method"] != "document" || v["edu_email"] != nil {
			t.Fatalf("verification %+v", v)
		}
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range emailRe.FindAllStringSubmatch(string(b), -1) {
			if !allowedDomains[strings.ToLower(m[1])] {
				t.Errorf("%s: real-looking e-mail domain %q", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBuild_SizesAndConsistency pins the demo's shape (BE-033) and the
// derived columns the services rely on.
func TestBuild_SizesAndConsistency(t *testing.T) {
	t.Parallel()
	ref := reference()
	p := build(t, ref, now)
	count := func(table string, keep func(map[string]any) bool) int {
		n := 0
		for _, r := range rows(p, table) {
			if keep == nil || keep(r) {
				n++
			}
		}
		return n
	}
	is := func(col string, v any) func(map[string]any) bool {
		return func(r map[string]any) bool { return r[col] == v }
	}
	students := count("users", is("signup_persona", "student"))
	minors := count("student_profiles", is("audience_eligible", false))
	for name, c := range map[string]struct{ got, min int }{
		"students":               {students, 300},
		"minor students":         {minors, 10},
		"clubs":                  {count("clubs", nil), 20},
		"sensitive clubs":        {count("clubs", is("is_sensitive", true)), 2},
		"companies":              {count("companies", nil), 3},
		"pending companies":      {count("organizations", is("status", "pending_review")), 1},
		"events":                 {count("events", nil), 30},
		"posts":                  {count("posts", nil), 200},
		"registrations":          {count("event_registrations", nil), 300},
		"check-ins":              {count("event_checkins", nil), 20},
		"platform admins":        {count("platform_role_assignments", is("role", "platform_admin")), 2},
		"finance admins":         {count("platform_role_assignments", is("role", "finance_admin")), 2},
		"dpo":                    {count("platform_role_assignments", is("role", "dpo")), 1},
		"targeted_post grants":   {count("entitlements", is("feature", "targeted_post")), 3},
		"segments":               {count("segments", nil), 3},
		"university reps":        {count("org_members", is("role", "university_rep")), 3},
		"club admins":            {count("org_members", is("role", "club_admin")), 20},
		"privacy notices":        {count("notice_versions", nil), 1},
		"waitlisted":             {count("event_registrations", is("status", "waitlisted")), 1},
		"pending (applications)": {count("event_registrations", is("status", "pending")), 1},
		"no-shows":               {count("event_registrations", is("status", "no_show")), 1},
	} {
		if c.got < c.min {
			t.Errorf("%s = %d, want ≥ %d", name, c.got, c.min)
		}
	}
	if n := count("platform_role_assignments", is("role", "platform_admin")); n != 2 {
		t.Errorf("platform admins = %d", n)
	}
	for _, s := range []string{"draft", "cancelled", "completed", "published"} {
		if count("events", is("status", s)) == 0 {
			t.Errorf("no %s event", s)
		}
	}
	// Minors: under 18 by birth date, never in an audience.
	for _, u := range rows(p, "users") {
		if u["email"] == seed.MinorEmail {
			if u["birth_date"] == nil {
				t.Fatal("the minor has no birth date")
			}
		}
	}
	// confirmed_count = confirmed + checked_in, never above capacity.
	seats := map[uuid.UUID]int{}
	for _, r := range rows(p, "event_registrations") {
		if r["status"] == "confirmed" || r["status"] == "checked_in" {
			seats[r["event_id"].(uuid.UUID)]++
		}
	}
	for _, e := range rows(p, "events") {
		id := e["id"].(uuid.UUID)
		if e["confirmed_count"].(int) != seats[id] {
			t.Errorf("event %s confirmed_count %v, want %d", e["slug"], e["confirmed_count"], seats[id])
		}
		if c, ok := e["capacity"].(*int); ok && c != nil && seats[id] > *c {
			t.Errorf("event %s over capacity", e["slug"])
		}
	}
	// like_count = the reactions of the post.
	likes := map[uuid.UUID]int{}
	for _, r := range rows(p, "post_reactions") {
		likes[r["post_id"].(uuid.UUID)]++
	}
	for _, post := range rows(p, "posts") {
		if lc, ok := post["like_count"].(int); ok && lc != likes[post["id"].(uuid.UUID)] {
			t.Errorf("post %s like_count %d, reactions %d", post["id"], lc, likes[post["id"].(uuid.UUID)])
		}
	}
	// Every ticketed registration has exactly one ticket; manual codes are
	// unique per event.
	codes := map[string]bool{}
	for _, tk := range rows(p, "registration_tickets") {
		k := tk["event_id"].(uuid.UUID).String() + tk["manual_code"].(string)
		if codes[k] {
			t.Fatalf("duplicate manual code %s", k)
		}
		codes[k] = true
	}
	d := p.Demo
	if d.MainClubOrgID == uuid.Nil() || d.CompanyOrgID == uuid.Nil() || d.SegmentID == uuid.Nil() || d.EntitlementID == uuid.Nil() ||
		d.PendingCompanyOrgID == uuid.Nil() || len(d.SensitiveClubOrgIDs) != 2 || d.MainUniversity.ID != ref.Universities[0].ID {
		t.Fatalf("demo = %+v", d)
	}
}

// TestBuild_Deterministic: the same seed and reference give the same rows
// with the same IDs, whenever the seed runs (the IDs never depend on the
// clock: a later run's ON CONFLICT DO NOTHING finds every row).
func TestBuild_Deterministic(t *testing.T) {
	t.Parallel()
	ref := reference()
	a, b := build(t, ref, now), build(t, ref, now.Add(36*time.Hour))
	ids := func(p *seed.Plan) []string {
		var out []string
		for _, r := range p.Rows {
			for i, c := range r.Cols {
				if c == "id" || (c == "user_id" && r.Table == "student_profiles") {
					out = append(out, r.Table+":"+r.Vals[i].(uuid.UUID).String())
				}
			}
		}
		return out
	}
	if !slices.Equal(ids(a), ids(b)) {
		t.Fatal("IDs depend on the clock")
	}
	if !slices.Equal(a.Strings(), build(t, ref, now).Strings()) {
		t.Fatal("same seed, different texts")
	}
	if _, err := seed.Build(seed.Reference{Universities: ref.Universities[:2], PlatformOrgID: ref.PlatformOrgID},
		seed.Options{Now: now, Tickets: tickets(t)}); err == nil || !strings.Contains(err.Error(), "koro seed reference") {
		t.Fatalf("two universities: %v", err)
	}
}

// TestSeedDemo_PrivilegedAccountsHaveNoPassword (06 H-66, T-287): with
// -password, the student, club and company accounts get the demo password
// but no account holding a platform role (platform_admin, finance_admin,
// dpo, moderator, support, talent_auditor) ever does: they are opened by a
// password reset and TOTP enrolment, so the shared demo password opens no
// privileged account without MFA.
func TestSeedDemo_PrivilegedAccountsHaveNoPassword(t *testing.T) {
	t.Parallel()
	p := build(t, reference(), now)
	pw := map[uuid.UUID]any{}
	email := map[uuid.UUID]string{}
	for _, u := range rows(p, "users") {
		id, ok := u["id"].(uuid.UUID)
		if !ok {
			t.Fatalf("users.id %T", u["id"])
		}
		pw[id], email[id] = u["password_hash"], u["email"].(string)
	}
	privileged := 0
	for _, r := range rows(p, "platform_role_assignments") {
		id, ok := r["user_id"].(uuid.UUID)
		if !ok {
			t.Fatalf("platform_role_assignments.user_id %T", r["user_id"])
		}
		privileged++
		if h, ok := pw[id]; !ok || h != nil {
			t.Errorf("%s (%v) has password hash %v, want none", email[id], r["role"], h)
		}
	}
	if privileged < 7 {
		t.Fatalf("%d platform role assignments, want the 7 demo staff", privileged)
	}
	for _, a := range p.Accounts {
		isStaff := slices.Contains([]string{"platform_admin", "finance_admin", "dpo", "moderator", "support", "talent_auditor"}, a.Role)
		if a.NoPassword != isStaff {
			t.Errorf("account %s (%s): NoPassword = %v", a.Email, a.Role, a.NoPassword)
		}
	}
	for _, e := range []string{seed.StudentEmail, seed.ClubAdminEmail, seed.CompanyEmail, seed.RepEmail} {
		found := false
		for id, m := range email {
			if m == e {
				found = true
				if h, _ := pw[id].(string); !strings.HasPrefix(h, "$argon2id$") {
					t.Errorf("%s has no demo password", e)
				}
			}
		}
		if !found {
			t.Errorf("%s is not seeded", e)
		}
	}
}
