//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/db/migrations"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_OrgMembersOneOwner (02 §7.3): a company has at most one active
// company_owner (partial unique index), a user has at most one open
// membership per org, the role must fit the org kind, and the status
// columns tell a consistent story. Ending the owner frees the seat.
func TestSchema_OrgMembersOneOwner(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company1, company2, club := w.org("company"), w.org("company"), w.org("club")
	u1, u2, u3, u4 := w.user(), w.user(), w.user(), w.user()
	const cols = `INSERT INTO org_members (org_id, org_kind, user_id, role, status, source, requested_at, joined_at, ended_at, end_reason) VALUES `
	active := func(org uuid.UUID, kind string, user uuid.UUID, role string) p1aCase {
		return p1aCase{kind + " " + role, cols + `($1, $2, $3, $4, 'active', 'admin', NULL, now(), NULL, NULL)`, []any{org, kind, user, role}, "", ""}
	}

	expectOK(t, w.d.Pool, []p1aCase{
		active(company1, "company", u1, "company_owner"),
		active(company2, "company", u2, "company_owner"), // another org may have its own owner
		active(company1, "company", u3, "company_admin"),
		active(company1, "company", u4, "recruiter"), // only the owner seat is limited
		active(club, "club", u1, "member"),
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"second active owner of one company", cols + `($1, 'company', $2, 'company_owner', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{company1, w.user()}, db.CodeUniqueViolation, "org_members_one_owner_uq"},
		{"promoting an existing member to a second owner", `UPDATE org_members SET role = 'company_owner' WHERE org_id = $1 AND user_id = $2`, []any{company1, u3}, db.CodeUniqueViolation, "org_members_one_owner_uq"},
		{"second open membership of one user", cols + `($1, 'company', $2, 'recruiter', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{company1, u3}, db.CodeUniqueViolation, "org_members_open_uq"},
		{"pending request next to an active membership", cols + `($1, 'company', $2, 'recruiter', 'pending', 'join_request', now(), NULL, NULL, NULL)`, []any{company1, u3}, db.CodeUniqueViolation, "org_members_open_uq"},
		{"club role in a company", cols + `($1, 'company', $2, 'club_admin', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{company1, w.user()}, db.CodeCheckViolation, "org_members_role_kind_chk"},
		{"company role in a club", cols + `($1, 'club', $2, 'recruiter', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_role_kind_chk"},
		{"membership stating the wrong org kind", cols + `($1, 'club', $2, 'member', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{company1, w.user()}, db.CodeForeignKeyViolation, "org_members_org_id_org_kind_fkey"},
		{"membership of an unknown org", cols + `($1, 'club', $2, 'member', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{uuid.NewV7(), w.user()}, db.CodeForeignKeyViolation, "org_members_org_id_org_kind_fkey"},
		{"invited by an unknown user", `INSERT INTO org_members (org_id, org_kind, user_id, role, status, source, joined_at, invited_by) VALUES ($1, 'club', $2, 'member', 'active', 'invitation', now(), $3)`, []any{club, w.user(), uuid.NewV7()}, db.CodeForeignKeyViolation, "org_members_invited_by_fkey"},
		{"approved by an unknown user", `INSERT INTO org_members (org_id, org_kind, user_id, role, status, source, joined_at, approved_by) VALUES ($1, 'club', $2, 'member', 'active', 'join_request', now(), $3)`, []any{club, w.user(), uuid.NewV7()}, db.CodeForeignKeyViolation, "org_members_approved_by_fkey"},
		{"membership of an unknown user", cols + `($1, 'club', $2, 'member', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{club, uuid.NewV7()}, db.CodeForeignKeyViolation, "org_members_user_id_fkey"},
		{"active without joined_at", cols + `($1, 'club', $2, 'member', 'active', 'admin', NULL, NULL, NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_active_chk"},
		{"active but already ended", cols + `($1, 'club', $2, 'member', 'active', 'admin', NULL, now(), now(), NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_active_chk"},
		{"ended without a reason", cols + `($1, 'club', $2, 'member', 'ended', 'admin', NULL, now(), now(), NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_ended_chk"},
		{"pending without requested_at", cols + `($1, 'club', $2, 'member', 'pending', 'join_request', NULL, NULL, NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_pending_chk"},
		{"unknown status", cols + `($1, 'club', $2, 'member', 'paused', 'admin', NULL, NULL, NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_status_chk"},
		{"unknown source", cols + `($1, 'club', $2, 'member', 'active', 'telepathy', NULL, now(), NULL, NULL)`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_source_chk"},
		{"unknown end reason", cols + `($1, 'club', $2, 'member', 'ended', 'admin', NULL, now(), now(), 'boredom')`, []any{club, w.user()}, db.CodeCheckViolation, "org_members_end_reason_chk"},
	})

	// A role change is an ended row plus a new row; the owner seat is free
	// again as soon as the owner's row is ended, but not before.
	expectFail(t, w.d.Pool, []p1aCase{
		{"new owner while the old one is active", cols + `($1, 'company', $2, 'company_owner', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{company1, w.user()}, db.CodeUniqueViolation, "org_members_one_owner_uq"},
	})
	w.exec(`UPDATE org_members SET status = 'ended', ended_at = now(), end_reason = 'role_changed' WHERE org_id = $1 AND role = 'company_owner'`, company1)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM org_members WHERE org_id = $1 AND role = 'company_owner'`, company1); got != "true" {
		t.Errorf("org_members.updated_at was not stamped")
	}
	expectOK(t, w.d.Pool, []p1aCase{
		{"new owner after the old one ended", cols + `($1, 'company', $2, 'company_owner', 'active', 'claim', NULL, now(), NULL, NULL)`, []any{company1, w.user()}, "", ""},
		{"the former owner may rejoin in another role", cols + `($1, 'company', $2, 'recruiter', 'active', 'admin', NULL, now(), NULL, NULL)`, []any{company1, u1}, "", ""},
	})
	if n := w.count(`SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 'company_owner' AND status = 'active'`, company1); n != 1 {
		t.Errorf("active owners of company1 = %d, want exactly 1", n)
	}
	// Ended rows are hard-deleted after the retention period.
	w.exec(`DELETE FROM org_members WHERE status = 'ended'`)
}

// TestSchema_OrganizationConstraints covers organizations (02 §7.1) and the
// platform org seeded by 000031.
func TestSchema_OrganizationConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)

	// 000031: exactly one platform org; re-running the seed keeps it that way.
	var name, slug, status string
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT name, slug, status FROM organizations WHERE kind = 'platform'`).Scan(&name, &slug, &status); err != nil {
		t.Fatalf("platform org: %v", err)
	}
	if name != "Koro" || slug != "koro" || status != "active" {
		t.Errorf("platform org = %q %q %q", name, slug, status)
	}
	seed, err := migrations.FS.ReadFile("000031_seed_platform_org.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	owner := testutil.OwnerConn(t, w.d)
	for i := range 2 {
		if _, err := owner.Exec(t.Context(), string(seed)); err != nil {
			t.Fatalf("seed run %d: %v", i+1, err)
		}
	}
	if n := w.count(`SELECT count(*) FROM organizations WHERE kind = 'platform'`); n != 1 {
		t.Errorf("platform orgs after re-running the seed = %d, want 1", n)
	}

	// UNIQUE (id, kind) is the target of every composite (org_id, org_kind) FK.
	if got := queryString(t, w.d, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'organizations_id_kind_uq'`); got != "UNIQUE (id, kind)" {
		t.Errorf("organizations_id_kind_uq = %q", got)
	}

	const cols = `INSERT INTO organizations (kind, name, slug, status, closed_at, website_url) VALUES `
	w.exec(`INSERT INTO organizations (kind, name, slug) VALUES ('club', 'Işık Astronomi Kulübü', 'isik-astronomi')`)
	expectFail(t, w.d.Pool, []p1aCase{
		{"second platform org", cols + `('platform', 'Koro 2', 'koro-2', 'active', NULL, NULL)`, nil, db.CodeUniqueViolation, "organizations_single_platform_uq"},
		{"duplicate slug", cols + `('club', 'Başka Kulüp', 'isik-astronomi', 'active', NULL, NULL)`, nil, db.CodeUniqueViolation, "organizations_slug_uq"},
		{"slug with capitals", cols + `('club', 'Kulüp', 'Isik-Astronomi', 'active', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_slug_chk"},
		{"slug with an underscore", cols + `('club', 'Kulüp', 'isik_astronomi', 'active', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_slug_chk"},
		{"slug with a double dash", cols + `('club', 'Kulüp', 'isik--astronomi', 'active', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_slug_chk"},
		{"slug over 80 characters", cols + `('club', 'Kulüp', '` + repeat("a", 81) + `', 'active', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_slug_chk"},
		{"one-letter name", cols + `('club', 'K', 'kisa-ad', 'active', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_name_chk"},
		{"unknown kind", cols + `('guild', 'Lonca', 'lonca', 'active', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_kind_chk"},
		{"unknown status", cols + `('club', 'Kulüp', 'kulup-x', 'archived', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_status_chk"},
		{"closed without closed_at", cols + `('club', 'Kulüp', 'kulup-y', 'closed', NULL, NULL)`, nil, db.CodeCheckViolation, "organizations_closed_chk"},
		{"closed_at on an open org", cols + `('club', 'Kulüp', 'kulup-z', 'active', now(), NULL)`, nil, db.CodeCheckViolation, "organizations_closed_chk"},
		{"plain http website", cols + `('club', 'Kulüp', 'kulup-w', 'active', NULL, 'http://example.test')`, nil, db.CodeCheckViolation, "organizations_website_url_chk"},
		{"javascript website", cols + `('club', 'Kulüp', 'kulup-v', 'active', NULL, 'javascript:alert(1)')`, nil, db.CodeCheckViolation, "organizations_website_url_chk"},
		{"over-long website", cols + `('club', 'Kulüp', 'kulup-u', 'active', NULL, 'https://example.test/` + repeat("p", 2040) + `')`, nil, db.CodeCheckViolation, "organizations_website_url_chk"},
		{"over-long description", `INSERT INTO organizations (kind, name, slug, description) VALUES ('club', 'Kulüp', 'kulup-t', $1)`, []any{repeat("d", 5001)}, db.CodeCheckViolation, "organizations_description_chk"},
		{"over-long city", `INSERT INTO organizations (kind, name, slug, city) VALUES ('club', 'Kulüp', 'kulup-s', $1)`, []any{repeat("c", 61)}, db.CodeCheckViolation, "organizations_city_chk"},
		{"unknown logo file", `INSERT INTO organizations (kind, name, slug, logo_file_id) VALUES ('club', 'Kulüp', 'kulup-r', $1)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "organizations_logo_file_id_fkey"},
		{"unknown verifier", `INSERT INTO organizations (kind, name, slug, verified_by) VALUES ('club', 'Kulüp', 'kulup-q', $1)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "organizations_verified_by_fkey"},
		{"unknown creator", `INSERT INTO organizations (kind, name, slug, created_by) VALUES ('club', 'Kulüp', 'kulup-p', $1)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "organizations_created_by_fkey"},
		{"app_rw cannot delete an org (closed, never deleted)", `DELETE FROM organizations WHERE slug = 'isik-astronomi'`, nil, db.CodeInsufficientPriv, ""},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"closed org", cols + `('club', 'Kapanan Kulüp', 'kapanan-kulup', 'closed', now(), 'https://example.test')`, nil, "", ""},
		{"ngo org (phase 2 vocabulary)", cols + `('ngo', 'Dernek', 'dernek', 'pending_review', NULL, NULL)`, nil, "", ""},
	})

	// Turkish-aware search over name/description: full text and trigram.
	w.exec(`UPDATE organizations SET description = 'Gökyüzü gözlemleri ve teleskop atölyeleri' WHERE slug = 'isik-astronomi'`)
	if n := w.count(`SELECT count(*) FROM organizations WHERE search_vector @@ websearch_to_tsquery('koro_tr', 'gözlemleri')`); n != 1 {
		t.Errorf("full-text search on organizations found %d, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM organizations WHERE koro_norm(name) % koro_norm('ISIK astronomi kulubu')`); n != 1 {
		t.Errorf("trigram search on organizations found %d, want 1", n)
	}
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM organizations WHERE slug = 'isik-astronomi'`); got != "true" {
		t.Errorf("organizations.updated_at was not stamped")
	}
}

// TestSchema_CompanyConstraints covers companies (02 §7.2): one row per
// company org, bound to an org of kind 'company' by the composite FK.
func TestSchema_CompanyConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, club := w.org("company"), w.org("club")
	expectOK(t, w.d.Pool, []p1aCase{
		{"company profile", `INSERT INTO companies (org_id, sector_code, size_band, email_domain, hq_city) VALUES ($1, 'software', '51-200', 'acme-yazilim.com.tr', 'İstanbul')`, []any{company}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"companies row for a club org", `INSERT INTO companies (org_id) VALUES ($1)`, []any{club}, db.CodeForeignKeyViolation, "companies_org_id_org_kind_fkey"},
		{"companies row for an unknown org", `INSERT INTO companies (org_id) VALUES ($1)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "companies_org_id_org_kind_fkey"},
		{"companies row claiming another kind", `INSERT INTO companies (org_id, org_kind) VALUES ($1, 'club')`, []any{club}, db.CodeCheckViolation, "companies_org_kind_chk"},
		{"second companies row of one org", `INSERT INTO companies (org_id) VALUES ($1)`, []any{company}, db.CodeUniqueViolation, "companies_pkey"},
		{"sector code with capitals", `UPDATE companies SET sector_code = 'Software' WHERE org_id = $1`, []any{company}, db.CodeCheckViolation, "companies_sector_code_chk"},
		{"unknown size band", `UPDATE companies SET size_band = '5' WHERE org_id = $1`, []any{company}, db.CodeCheckViolation, "companies_size_band_chk"},
		{"e-mail domain with a scheme", `UPDATE companies SET email_domain = 'https://acme.com' WHERE org_id = $1`, []any{company}, db.CodeCheckViolation, "companies_email_domain_chk"},
		{"e-mail domain without a TLD", `UPDATE companies SET email_domain = 'acme' WHERE org_id = $1`, []any{company}, db.CodeCheckViolation, "companies_email_domain_chk"},
		{"app_rw cannot delete a company row", `DELETE FROM companies WHERE org_id = $1`, []any{company}, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE companies SET domain_verified_at = now() WHERE org_id = $1`, company)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM companies WHERE org_id = $1`, company); got != "true" {
		t.Errorf("companies.updated_at was not stamped")
	}
}

// TestSchema_OrgInvitationConstraints covers org_invitations (02 §7.4): the
// role fits the org kind and company_owner / platform roles cannot be
// invited, one open invitation per org and address, the outcome columns.
func TestSchema_OrgInvitationConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, club, platform := w.org("company"), w.org("club"), w.platformOrg()
	inviter, accepter := w.user(), w.user()
	const cols = `INSERT INTO org_invitations (org_id, org_kind, email, email_normalized, role, key_id, token_hash, invited_by, expires_at, accepted_at, accepted_user_id, revoked_at) VALUES `
	invite := func(org uuid.UUID, kind, email, role, token string) p1aCase {
		return p1aCase{"invite " + email + " as " + role, cols + `($1, $2, $3, lower($3), $4, 'k1', $5, $6, now() + interval '7 days', NULL, NULL, NULL)`,
			[]any{org, kind, email, role, bytes32(token), inviter}, "", ""}
	}
	failing := func(c p1aCase, state, constraint string) p1aCase { c.state, c.constraint = state, constraint; return c }

	expectOK(t, w.d.Pool, []p1aCase{
		invite(company, "company", "Ayse@Example.test", "recruiter", "i1"),
		invite(company, "company", "can@example.test", "company_admin", "i2"),
		invite(club, "club", "ayse@example.test", "club_admin", "i3"), // the same address in another org
	})
	expectFail(t, w.d.Pool, []p1aCase{
		failing(invite(company, "company", "AYSE@example.test", "recruiter", "i4"), db.CodeUniqueViolation, "org_invitations_open_uq"),
		failing(invite(company, "company", "owner@example.test", "company_owner", "i5"), db.CodeCheckViolation, "org_invitations_role_kind_chk"),
		failing(invite(company, "company", "wrong@example.test", "member", "i6"), db.CodeCheckViolation, "org_invitations_role_kind_chk"),
		failing(invite(platform, "platform", "staff@example.test", "staff", "i7"), db.CodeCheckViolation, "org_invitations_role_kind_chk"),
		failing(invite(company, "club", "kind@example.test", "member", "i8"), db.CodeForeignKeyViolation, "org_invitations_org_id_org_kind_fkey"),
		failing(invite(company, "company", "dup-token@example.test", "recruiter", "i1"), db.CodeUniqueViolation, "org_invitations_token_hash_uq"),
		{"short token digest", cols + `($1, 'company', 'a@example.test', 'a@example.test', 'recruiter', 'k1', '\x01', $2, now(), NULL, NULL, NULL)`, []any{company, inviter}, db.CodeCheckViolation, "org_invitations_token_hash_chk"},
		{"over-long address", cols + `($1, 'company', $3, 'long@example.test', 'recruiter', 'k1', $4, $2, now(), NULL, NULL, NULL)`, []any{company, inviter, repeat("a", 255), bytes32("i9")}, db.CodeCheckViolation, "org_invitations_email_chk"},
		{"accepted without the accepting user", cols + `($1, 'company', 'b@example.test', 'b@example.test', 'recruiter', 'k1', $3, $2, now(), now(), NULL, NULL)`, []any{company, inviter, bytes32("i10")}, db.CodeCheckViolation, "org_invitations_accepted_chk"},
		{"accepting user without a time", cols + `($1, 'company', 'c@example.test', 'c@example.test', 'recruiter', 'k1', $3, $2, now(), NULL, $4, NULL)`, []any{company, inviter, bytes32("i11"), accepter}, db.CodeCheckViolation, "org_invitations_accepted_chk"},
		{"accepted and revoked", cols + `($1, 'company', 'd@example.test', 'd@example.test', 'recruiter', 'k1', $3, $2, now(), now(), $4, now())`, []any{company, inviter, bytes32("i12"), accepter}, db.CodeCheckViolation, "org_invitations_outcome_chk"},
		{"unknown inviter", cols + `($1, 'company', 'e@example.test', 'e@example.test', 'recruiter', 'k1', $3, $2, now(), NULL, NULL, NULL)`, []any{company, uuid.NewV7(), bytes32("i13")}, db.CodeForeignKeyViolation, "org_invitations_invited_by_fkey"},
		{"unknown accepting user", cols + `($1, 'company', 'f@example.test', 'f@example.test', 'recruiter', 'k1', $3, $2, now(), now(), $4, NULL)`, []any{company, inviter, bytes32("i14"), uuid.NewV7()}, db.CodeForeignKeyViolation, "org_invitations_accepted_user_id_fkey"},
	})
	// A revoked or accepted invitation frees the (org, address) slot.
	w.exec(`UPDATE org_invitations SET revoked_at = now() WHERE org_id = $1 AND email_normalized = 'ayse@example.test'`, company)
	expectOK(t, w.d.Pool, []p1aCase{
		invite(company, "company", "ayse@example.test", "recruiter", "i15"),
	})
	w.exec(`UPDATE org_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE org_id = $1 AND email_normalized = 'can@example.test'`, company, accepter)
	expectOK(t, w.d.Pool, []p1aCase{
		invite(company, "company", "can@example.test", "recruiter", "i16"),
	})
	w.exec(`DELETE FROM org_invitations WHERE accepted_at IS NOT NULL OR revoked_at IS NOT NULL`) // retention: hard delete 30 days after the outcome
}
