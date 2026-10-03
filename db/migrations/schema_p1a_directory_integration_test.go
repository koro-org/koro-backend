//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_UniversityConstraints covers universities (02 §8.1): 1:1 with
// an org of kind 'university', unique YÖK code, the website scheme rule,
// and the Turkish search columns (full text and trigram).
func TestSchema_UniversityConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uniOrg, clubOrg := w.org("university"), w.org("club")
	const cols = `INSERT INTO universities (org_id, yok_code, name, short_name, city, type, website_url) VALUES `

	expectOK(t, w.d.Pool, []p1aCase{
		{"university", cols + `($1, '100001', 'İstanbul Teknik Üniversitesi', 'İTÜ', 'İstanbul', 'state', 'https://www.itu.edu.tr')`, []any{uniOrg}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"university row for a club org", cols + `($1, '100002', 'Sahte Üniversite', NULL, 'Ankara', 'state', NULL)`, []any{clubOrg}, db.CodeForeignKeyViolation, "universities_org_id_org_kind_fkey"},
		{"university row for an unknown org", cols + `($1, '100003', 'Sahte Üniversite', NULL, 'Ankara', 'state', NULL)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "universities_org_id_org_kind_fkey"},
		{"university claiming another kind", `INSERT INTO universities (org_id, org_kind, yok_code, name, city, type) VALUES ($1, 'club', '100004', 'Sahte', 'Ankara', 'state')`, []any{clubOrg}, db.CodeCheckViolation, "universities_org_kind_chk"},
		{"second university for one org", cols + `($1, '100005', 'Başka Üniversite', NULL, 'Ankara', 'state', NULL)`, []any{uniOrg}, db.CodeUniqueViolation, "universities_org_id_uq"},
		{"duplicate YÖK code", cols + `($1, '100001', 'Başka Üniversite', NULL, 'Ankara', 'state', NULL)`, []any{w.org("university")}, db.CodeUniqueViolation, "universities_yok_code_uq"},
		{"unknown type", cols + `($1, '100006', 'Üniversite', NULL, 'Ankara', 'private', NULL)`, []any{w.org("university")}, db.CodeCheckViolation, "universities_type_chk"},
		{"ftp website", cols + `($1, '100007', 'Üniversite', NULL, 'Ankara', 'state', 'ftp://itu.edu.tr')`, []any{w.org("university")}, db.CodeCheckViolation, "universities_website_url_chk"},
		{"javascript website", cols + `($1, '100008', 'Üniversite', NULL, 'Ankara', 'state', 'javascript:alert(1)')`, []any{w.org("university")}, db.CodeCheckViolation, "universities_website_url_chk"},
		{"missing city", `INSERT INTO universities (org_id, yok_code, name, type) VALUES ($1, '100009', 'Üniversite', 'state')`, []any{w.org("university")}, codeNotNull, ""},
		{"app_rw cannot delete a university (deactivated, never deleted)", `DELETE FROM universities WHERE yok_code = '100001'`, nil, db.CodeInsufficientPriv, ""},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"plain http website is accepted for universities", cols + `($1, '100010', 'Boğaziçi Üniversitesi', NULL, 'İstanbul', 'state', 'http://www.boun.edu.tr')`, []any{w.org("university")}, "", ""},
		{"deactivate", `UPDATE universities SET is_active = false WHERE yok_code = '100010'`, nil, "", ""},
	})
	if n := w.count(`SELECT count(*) FROM universities WHERE search_vector @@ websearch_to_tsquery('koro_tr', 'istanbul')`); n != 2 {
		t.Errorf("full-text search 'istanbul' found %d universities, want 2 (city is part of the vector)", n)
	}
	// Typo/accent-tolerant search: the best trigram match for an ASCII query is the right university.
	if got := queryString(t, w.d, `SELECT yok_code FROM universities WHERE koro_norm(name) % koro_norm($1)
		ORDER BY similarity(koro_norm(name), koro_norm($1)) DESC LIMIT 1`, "ISTANBUL TEKNIK UNIVERSITESI"); got != "100001" {
		t.Errorf("trigram search found university %q, want 100001", got)
	}
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM universities WHERE yok_code = '100010'`); got != "true" {
		t.Errorf("universities.updated_at was not stamped")
	}
}

// TestSchema_UniversityEmailDomainConstraints covers university_email_domains
// (02 §8.2): only well-formed lower-case .edu.tr domains.
func TestSchema_UniversityEmailDomainConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni, _ := w.university()
	admin := w.user()
	const cols = `INSERT INTO university_email_domains (domain, university_id, domain_kind, verified_by) VALUES `
	for _, domain := range []string{"itu.edu.tr", "ogr.itu.edu.tr", "cs-dept.ogr.itu.edu.tr", "boun.edu.tr"} {
		expectOK(t, w.d.Pool, []p1aCase{{"domain " + domain, cols + `($1, $2, 'mixed', $3)`, []any{domain, uni, admin}, "", ""}})
	}
	var bad []p1aCase
	for _, domain := range []string{
		"edu.tr", "itu.edu.tr.evil.com", "ITU.edu.tr", "itu.com.tr", "itu.edu.trx", "https://itu.edu.tr", "a b.edu.tr", ".itu.edu.tr", "itu..edu.tr", "itü.edu.tr", "",
	} {
		bad = append(bad, p1aCase{"domain " + domain, cols + `($1, $2, 'mixed', NULL)`, []any{domain, uni}, db.CodeCheckViolation, "university_email_domains_domain_chk"})
	}
	expectFail(t, w.d.Pool, append(bad, []p1aCase{
		{"duplicate domain", cols + `('itu.edu.tr', $1, 'mixed', NULL)`, []any{uni}, db.CodeUniqueViolation, "university_email_domains_pkey"},
		{"unknown domain kind", cols + `('metu.edu.tr', $1, 'faculty', NULL)`, []any{uni}, db.CodeCheckViolation, "university_email_domains_domain_kind_chk"},
		{"domain of an unknown university", cols + `('metu.edu.tr', $1, 'mixed', NULL)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "university_email_domains_university_id_fkey"},
		{"unknown verifier", cols + `('metu.edu.tr', $1, 'mixed', $2)`, []any{uni, uuid.NewV7()}, db.CodeForeignKeyViolation, "university_email_domains_verified_by_fkey"},
		{"app_rw cannot delete a domain", `DELETE FROM university_email_domains WHERE domain = 'itu.edu.tr'`, nil, db.CodeInsufficientPriv, ""},
	}...))
	w.exec(`UPDATE university_email_domains SET is_active = false WHERE domain = 'boun.edu.tr'`)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM university_email_domains WHERE domain = 'boun.edu.tr'`); got != "true" {
		t.Errorf("university_email_domains.updated_at was not stamped")
	}
}

// TestSchema_FacultyAndDepartmentConstraints covers faculties and
// departments (02 §8.3, §8.4): unique names per university, unique YÖK
// program code per university (several NULLs allowed), degree/duration
// vocabularies, and Turkish search on department names.
func TestSchema_FacultyAndDepartmentConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni1, _ := w.university()
	uni2, _ := w.university()
	fac := uuid.NewV7()

	w.exec(`INSERT INTO faculties (id, university_id, name, yok_code) VALUES ($1, $2, 'Mühendislik Fakültesi', 'F1')`, fac, uni1)
	expectOK(t, w.d.Pool, []p1aCase{
		{"same faculty name at another university", `INSERT INTO faculties (university_id, name) VALUES ($1, 'Mühendislik Fakültesi')`, []any{uni2}, "", ""},
		{"another faculty", `INSERT INTO faculties (university_id, name) VALUES ($1, 'Fen-Edebiyat Fakültesi')`, []any{uni1}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"duplicate faculty name at one university", `INSERT INTO faculties (university_id, name) VALUES ($1, 'Mühendislik Fakültesi')`, []any{uni1}, db.CodeUniqueViolation, "faculties_university_name_uq"},
		{"faculty of an unknown university", `INSERT INTO faculties (university_id, name) VALUES ($1, 'Hayalet Fakülte')`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "faculties_university_id_fkey"},
		{"app_rw cannot delete a faculty", `DELETE FROM faculties WHERE id = $1`, []any{fac}, db.CodeInsufficientPriv, ""},
	})

	const cols = `INSERT INTO departments (university_id, faculty_id, name, yok_program_code, degree_level, duration_years) VALUES `
	expectOK(t, w.d.Pool, []p1aCase{
		{"department with a program code", cols + `($1, $2, 'Bilgisayar Mühendisliği', '1001', 'bachelor', 4)`, []any{uni1, fac}, "", ""},
		{"department without a faculty or code", cols + `($1, NULL, 'Açık Öğretim', NULL, 'associate', 2)`, []any{uni1}, "", ""},
		{"a second department without a code", cols + `($1, NULL, 'Uzaktan Eğitim', NULL, 'associate', 2)`, []any{uni1}, "", ""},
		{"same program code at another university", cols + `($1, NULL, 'Bilgisayar Mühendisliği', '1001', 'bachelor', 4)`, []any{uni2}, "", ""},
		{"doctorate of seven years", cols + `($1, NULL, 'Tıp', '2002', 'doctorate', 7)`, []any{uni1}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"duplicate program code at one university", cols + `($1, NULL, 'Başka Bölüm', '1001', 'master', 2)`, []any{uni1}, db.CodeUniqueViolation, "departments_yok_uq"},
		{"unknown degree level", cols + `($1, NULL, 'Bölüm', NULL, 'diploma', 2)`, []any{uni1}, db.CodeCheckViolation, "departments_degree_level_chk"},
		{"duration of eight years", cols + `($1, NULL, 'Bölüm', NULL, 'bachelor', 8)`, []any{uni1}, db.CodeCheckViolation, "departments_duration_years_chk"},
		{"duration of zero", cols + `($1, NULL, 'Bölüm', NULL, 'bachelor', 0)`, []any{uni1}, db.CodeCheckViolation, "departments_duration_years_chk"},
		{"unknown faculty", cols + `($1, $2, 'Bölüm', NULL, 'bachelor', 4)`, []any{uni1, uuid.NewV7()}, db.CodeForeignKeyViolation, "departments_faculty_id_fkey"},
		{"department of an unknown university", cols + `($1, NULL, 'Bölüm', NULL, 'bachelor', 4)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "departments_university_id_fkey"},
		{"app_rw cannot delete a department", `DELETE FROM departments WHERE yok_program_code = '1001'`, nil, db.CodeInsufficientPriv, ""},
	})
	if n := w.count(`SELECT count(*) FROM departments WHERE koro_norm(name) % koro_norm('BILGISAYAR MUHENDISLIGI')`); n != 2 {
		t.Errorf("trigram search on departments found %d, want 2 (one per university)", n)
	}
	// Turkish collation on the name columns: c < ç < d, ı < i (02 §0.4).
	uni3, _ := w.university()
	for _, name := range []string{"İpek Fakültesi", "Işık Fakültesi", "Çiçek Fakültesi", "Cam Fakültesi", "Dalga Fakültesi"} {
		w.exec(`INSERT INTO faculties (university_id, name) VALUES ($1, $2)`, uni3, name)
	}
	const want = "Cam Fakültesi,Çiçek Fakültesi,Dalga Fakültesi,Işık Fakültesi,İpek Fakültesi"
	if got := queryString(t, w.d, `SELECT string_agg(name, ',' ORDER BY name) FROM faculties WHERE university_id = $1`, uni3); got != want {
		t.Errorf("faculties in Turkish order = %q, want %q", got, want)
	}
}

// TestSchema_ClubConstraints covers clubs (02 §8.5): bound to an org of
// kind 'club'; the sensitivity flag, its reason and its stamp move
// together, and religious/political/identity clubs are always sensitive.
func TestSchema_ClubConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni, _ := w.university()
	admin := w.user()
	const cols = `INSERT INTO clubs (org_id, university_id, category, is_sensitive, sensitive_reason, sensitivity_set_by, sensitivity_set_at) VALUES `
	newClubOrg := func() uuid.UUID { return w.org("club") }

	expectOK(t, w.d.Pool, []p1aCase{
		{"ordinary club", cols + `($1, $2, 'technology', false, NULL, NULL, NULL)`, []any{newClubOrg(), uni}, "", ""},
		{"sensitive religious club", cols + `($1, $2, 'religious', true, 'religion', $3, now())`, []any{newClubOrg(), uni, admin}, "", ""},
		{"ordinary category flagged sensitive by an admin", cols + `($1, $2, 'sports', true, 'health', $3, now())`, []any{newClubOrg(), uni, admin}, "", ""},
		{"sensitive identity club without a setter (import)", cols + `($1, $2, 'identity', true, 'lgbtiq', NULL, now())`, []any{newClubOrg(), uni}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"religious club not flagged", cols + `($1, $2, 'religious', false, NULL, NULL, NULL)`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_sensitive_category_chk"},
		{"political club not flagged", cols + `($1, $2, 'political', false, NULL, NULL, NULL)`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_sensitive_category_chk"},
		{"identity club not flagged", cols + `($1, $2, 'identity', false, NULL, NULL, NULL)`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_sensitive_category_chk"},
		{"flag without a reason", cols + `($1, $2, 'sports', true, NULL, NULL, now())`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_sensitive_reason_set_chk"},
		{"reason without the flag", cols + `($1, $2, 'sports', false, 'health', NULL, NULL)`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_sensitive_reason_set_chk"},
		{"flag without the stamp", cols + `($1, $2, 'sports', true, 'health', $3, NULL)`, []any{newClubOrg(), uni, admin}, db.CodeCheckViolation, "clubs_sensitivity_stamp_chk"},
		{"stamp without the flag", cols + `($1, $2, 'sports', false, NULL, NULL, now())`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_sensitivity_stamp_chk"},
		{"unknown sensitivity reason", cols + `($1, $2, 'sports', true, 'hobby', NULL, now())`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_sensitive_reason_chk"},
		{"unknown category", cols + `($1, $2, 'gaming', false, NULL, NULL, NULL)`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_category_chk"},
		{"club row for a company org", cols + `($1, $2, 'academic', false, NULL, NULL, NULL)`, []any{w.org("company"), uni}, db.CodeForeignKeyViolation, "clubs_org_id_org_kind_fkey"},
		{"club row claiming another kind", `INSERT INTO clubs (org_id, org_kind, university_id, category) VALUES ($1, 'company', $2, 'academic')`, []any{w.org("company"), uni}, db.CodeCheckViolation, "clubs_org_kind_chk"},
		{"club of an unknown university", cols + `($1, $2, 'academic', false, NULL, NULL, NULL)`, []any{newClubOrg(), uuid.NewV7()}, db.CodeForeignKeyViolation, "clubs_university_id_fkey"},
		{"unknown sensitivity setter", cols + `($1, $2, 'sports', true, 'health', $3, now())`, []any{newClubOrg(), uni, uuid.NewV7()}, db.CodeForeignKeyViolation, "clubs_sensitivity_set_by_fkey"},
		{"unknown join policy", `INSERT INTO clubs (org_id, university_id, category, join_policy) VALUES ($1, $2, 'academic', 'bribe')`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_join_policy_chk"},
		{"unknown claim status", `INSERT INTO clubs (org_id, university_id, category, claim_status) VALUES ($1, $2, 'academic', 'stolen')`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_claim_status_chk"},
		{"founded before 1900", `INSERT INTO clubs (org_id, university_id, category, founded_year) VALUES ($1, $2, 'academic', 1899)`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_founded_year_chk"},
		{"founded after 2100", `INSERT INTO clubs (org_id, university_id, category, founded_year) VALUES ($1, $2, 'academic', 2101)`, []any{newClubOrg(), uni}, db.CodeCheckViolation, "clubs_founded_year_chk"},
		{"app_rw cannot delete a club row", `DELETE FROM clubs WHERE category = 'technology'`, nil, db.CodeInsufficientPriv, ""},
	})

	// A club turning sensitive moves flag, reason and stamp in one update; a
	// half update is refused. The partial index of non-sensitive clubs follows.
	club := w.club(uni)
	expectFail(t, w.d.Pool, []p1aCase{
		{"flag set without a reason", `UPDATE clubs SET is_sensitive = true, sensitivity_set_at = now() WHERE org_id = $1`, []any{club}, db.CodeCheckViolation, "clubs_sensitive_reason_set_chk"},
		{"flag set without the stamp", `UPDATE clubs SET is_sensitive = true, sensitive_reason = 'health' WHERE org_id = $1`, []any{club}, db.CodeCheckViolation, "clubs_sensitivity_stamp_chk"},
		{"religious category without the flag", `UPDATE clubs SET category = 'religious' WHERE org_id = $1`, []any{club}, db.CodeCheckViolation, "clubs_sensitive_category_chk"},
	})
	w.exec(`UPDATE clubs SET is_sensitive = true, sensitive_reason = 'political_opinion', sensitivity_set_by = $2, sensitivity_set_at = now(), category = 'political' WHERE org_id = $1`, club, admin)
	if n := w.count(`SELECT count(*) FROM clubs WHERE university_id = $1 AND NOT is_sensitive`, uni); n != 1 {
		t.Errorf("non-sensitive clubs = %d, want 1 (the 'technology' club)", n)
	}
	if got := queryString(t, w.d, `SELECT social_links::text FROM clubs WHERE org_id = $1`, club); got != "{}" {
		t.Errorf("social_links default = %q", got)
	}
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM clubs WHERE org_id = $1`, club); got != "true" {
		t.Errorf("clubs.updated_at was not stamped")
	}
}

// TestSchema_ClubClaimConstraints covers club_claims (02 §8.6): one pending
// claim per club and claimant, the decision columns move with the status,
// and nobody decides their own claim.
func TestSchema_ClubClaimConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni, _ := w.university()
	club := w.club(uni)
	claimant, other, admin := w.user(), w.user(), w.user()
	const cols = `INSERT INTO club_claims (club_org_id, claimant_user_id, status, role_in_club, reviewed_by, reviewed_at) VALUES `

	expectOK(t, w.d.Pool, []p1aCase{
		{"pending claim", cols + `($1, $2, 'pending', 'Başkan', NULL, NULL)`, []any{club, claimant}, "", ""},
		{"another claimant of the same club", cols + `($1, $2, 'pending', 'YK üyesi', NULL, NULL)`, []any{club, other}, "", ""},
		{"decided claim kept as history", cols + `($1, $2, 'rejected', 'Başkan', $3, now())`, []any{club, other, admin}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"second pending claim of one claimant", cols + `($1, $2, 'pending', 'Başkan', NULL, NULL)`, []any{club, claimant}, db.CodeUniqueViolation, "club_claims_pending_uq"},
		{"approved without a reviewer", cols + `($1, $2, 'approved', 'Başkan', NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "club_claims_review_chk"},
		{"approved without a review time", cols + `($1, $2, 'approved', 'Başkan', $3, NULL)`, []any{club, w.user(), admin}, db.CodeCheckViolation, "club_claims_review_chk"},
		{"pending but already reviewed", cols + `($1, $2, 'pending', 'Başkan', $3, now())`, []any{club, w.user(), admin}, db.CodeCheckViolation, "club_claims_review_chk"},
		{"withdrawn with a reviewer", cols + `($1, $2, 'withdrawn', 'Başkan', $3, now())`, []any{club, w.user(), admin}, db.CodeCheckViolation, "club_claims_review_chk"},
		{"nobody decides their own claim", cols + `($1, $2, 'approved', 'Başkan', $2, now())`, []any{club, claimant}, db.CodeCheckViolation, "club_claims_reviewer_chk"},
		{"unknown status", cols + `($1, $2, 'lost', 'Başkan', NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "club_claims_status_chk"},
		{"one-letter role", cols + `($1, $2, 'pending', 'B', NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "club_claims_role_in_club_chk"},
		{"over-long role", cols + `($1, $2, 'pending', $3, NULL, NULL)`, []any{club, w.user(), repeat("r", 101)}, db.CodeCheckViolation, "club_claims_role_in_club_chk"},
		{"over-long evidence text", `INSERT INTO club_claims (club_org_id, claimant_user_id, role_in_club, evidence_text) VALUES ($1, $2, 'Başkan', $3)`, []any{club, w.user(), repeat("e", 2001)}, db.CodeCheckViolation, "club_claims_evidence_text_chk"},
		{"over-long decision note", `INSERT INTO club_claims (club_org_id, claimant_user_id, role_in_club, decision_note) VALUES ($1, $2, 'Başkan', $3)`, []any{club, w.user(), repeat("n", 1001)}, db.CodeCheckViolation, "club_claims_decision_note_chk"},
		{"claim of an org that is not a club", cols + `($1, $2, 'pending', 'Başkan', NULL, NULL)`, []any{w.org("company"), w.user()}, db.CodeForeignKeyViolation, "club_claims_club_org_id_fkey"},
		{"claim of an unknown user", cols + `($1, $2, 'pending', 'Başkan', NULL, NULL)`, []any{club, uuid.NewV7()}, db.CodeForeignKeyViolation, "club_claims_claimant_user_id_fkey"},
		{"unknown evidence file", `INSERT INTO club_claims (club_org_id, claimant_user_id, role_in_club, evidence_file_id) VALUES ($1, $2, 'Başkan', $3)`, []any{club, w.user(), uuid.NewV7()}, db.CodeForeignKeyViolation, "club_claims_evidence_file_id_fkey"},
		{"unknown reviewer", cols + `($1, $2, 'approved', 'Başkan', $3, now())`, []any{club, w.user(), uuid.NewV7()}, db.CodeForeignKeyViolation, "club_claims_reviewed_by_fkey"},
	})
	// The decision frees the slot; the same claimant may claim again later.
	w.exec(`UPDATE club_claims SET status = 'approved', reviewed_by = $2, reviewed_at = now(), decision_note = 'Belgeler uygun' WHERE club_org_id = $1 AND claimant_user_id = $3 AND status = 'pending'`, club, admin, claimant)
	expectOK(t, w.d.Pool, []p1aCase{
		{"new pending claim after the decision", cols + `($1, $2, 'pending', 'Başkan', NULL, NULL)`, []any{club, claimant}, "", ""},
	})
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM club_claims WHERE club_org_id = $1 AND status = 'approved'`, club); got != "true" {
		t.Errorf("club_claims.updated_at was not stamped")
	}
	w.exec(`DELETE FROM club_claims WHERE status IN ('approved','rejected','withdrawn')`) // retention: hard delete 1 year after the decision
}

// TestSchema_SkillConstraints covers skills (02 §8.7).
func TestSchema_SkillConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	const cols = `INSERT INTO skills (slug, name_tr, name_en, category) VALUES `
	expectOK(t, w.d.Pool, []p1aCase{
		{"skill", cols + `('go-programlama', 'Go Programlama', 'Go Programming', 'technical')`, nil, "", ""},
		{"skill without an English name", cols + `('takim-calismasi', 'Takım Çalışması', NULL, 'soft')`, nil, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"duplicate slug", cols + `('go-programlama', 'Başka Ad', NULL, 'technical')`, nil, db.CodeUniqueViolation, "skills_slug_uq"},
		{"slug with capitals", cols + `('Go-Programlama', 'Ad', NULL, 'technical')`, nil, db.CodeCheckViolation, "skills_slug_chk"},
		{"slug with a Turkish letter", cols + `('takım-çalışması', 'Ad', NULL, 'soft')`, nil, db.CodeCheckViolation, "skills_slug_chk"},
		{"slug with an underscore", cols + `('takim_calismasi', 'Ad', NULL, 'soft')`, nil, db.CodeCheckViolation, "skills_slug_chk"},
		{"unknown category", cols + `('yetenek', 'Ad', NULL, 'magic')`, nil, db.CodeCheckViolation, "skills_category_chk"},
		{"missing Turkish name", `INSERT INTO skills (slug, category) VALUES ('adsiz', 'other')`, nil, codeNotNull, ""},
		{"app_rw cannot delete a skill (deactivated, never deleted)", `DELETE FROM skills WHERE slug = 'go-programlama'`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE skills SET is_active = false WHERE slug = 'go-programlama'`)
	if n := w.count(`SELECT count(*) FROM skills WHERE koro_norm(name_tr) % koro_norm('takim calismasi')`); n != 1 {
		t.Errorf("trigram search found %d skills, want 1", n)
	}
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM skills WHERE slug = 'go-programlama'`); got != "true" {
		t.Errorf("skills.updated_at was not stamped")
	}
}
