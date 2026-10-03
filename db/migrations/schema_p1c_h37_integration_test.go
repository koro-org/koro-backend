//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/db"
)

// TestSchema_H37StateChecksRejectPartialRows (06 H-37, T-131): the BE-009
// state CHECKs that were written as (state) = (A AND B) are split, in place
// before the pilot. A row that is not in the state may no longer carry half
// of the state's columns; the rows that were valid before stay valid. Each
// "partial" case below was accepted by the old CHECK (the mutation run of
// BE-011 restored the old CHECKs and saw exactly these cases pass).
func TestSchema_H37StateChecksRejectPartialRows(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation

	// 000021 edu_verifications: every e-mail column belongs to the e-mail
	// method. Each case has its own user: one accepted pending row must not
	// mask the next case behind edu_verifications_one_pending_uq.
	user, file := w.user(), w.file("edu_document")
	edu := `INSERT INTO edu_verifications (user_id, method, document_file_id, edu_email, edu_email_normalized, email_domain) VALUES ($1, $2, $3, $4, $5, $6)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"partial: a document verification with an e-mail address only", edu, []any{w.user(), "document", file, "a@x.edu.tr", nil, nil}, chk, "edu_verifications_email_method_chk"},
		{"partial: a document verification with a domain only", edu, []any{w.user(), "document", file, nil, nil, "x.edu.tr"}, chk, "edu_verifications_email_method_chk"},
		{"partial: a document verification with two of the three", edu, []any{w.user(), "document", file, "a@x.edu.tr", "a@x.edu.tr", nil}, chk, "edu_verifications_email_method_chk"},
		{"an e-mail verification without the domain", edu, []any{w.user(), "edu_email", nil, "a@x.edu.tr", "a@x.edu.tr", nil}, chk, "edu_verifications_email_method_chk"},
		{"an e-mail verification with the address only", edu, []any{w.user(), "edu_email", nil, "a@x.edu.tr", nil, nil}, chk, "edu_verifications_email_method_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"a document verification", edu, []any{user, "document", file, nil, nil, nil}, "", ""},
		{"an e-mail verification", edu, []any{w.user(), "edu_email", nil, "b@x.edu.tr", "b@x.edu.tr", "x.edu.tr"}, "", ""},
	})

	// 000042 club_claims: the reviewer and the review time belong to a decision.
	uni, _ := w.university()
	club, admin := w.club(uni), w.user()
	claim := `INSERT INTO club_claims (club_org_id, claimant_user_id, status, role_in_club, reviewed_by, reviewed_at) VALUES ($1, $2, $3, 'Başkan', $4, $5)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"partial: a pending claim with a reviewer only", claim, []any{club, w.user(), "pending", admin, nil}, chk, "club_claims_review_chk"},
		{"partial: a pending claim with a review time only", claim, []any{club, w.user(), "pending", nil, "2026-10-03T00:00:00Z"}, chk, "club_claims_review_chk"},
		{"partial: a withdrawn claim with a review time only", claim, []any{club, w.user(), "withdrawn", nil, "2026-10-03T00:00:00Z"}, chk, "club_claims_review_chk"},
		{"an approved claim without a review time", claim, []any{club, w.user(), "approved", admin, nil}, chk, "club_claims_review_chk"},
		{"a rejected claim without a reviewer", claim, []any{club, w.user(), "rejected", nil, "2026-10-03T00:00:00Z"}, chk, "club_claims_review_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"a pending claim", claim, []any{club, w.user(), "pending", nil, nil}, "", ""},
		{"a withdrawn claim", claim, []any{club, w.user(), "withdrawn", nil, nil}, "", ""},
		{"a rejected claim", claim, []any{club, w.user(), "rejected", admin, "2026-10-03T00:00:00Z"}, "", ""},
	})

	// 000034 org_members: ended_at and end_reason belong to 'ended'; an
	// active membership has joined and not ended; a pending one has not joined.
	org := w.org("club")
	member := `INSERT INTO org_members (org_id, org_kind, user_id, role, status, source, requested_at, joined_at, ended_at, end_reason)
		VALUES ($1, 'club', $2, 'member', $3, 'join_request', now(), $4, $5, $6)`
	when := "2026-10-03T00:00:00Z"
	expectFail(t, w.d.Pool, []p1aCase{
		{"partial: a pending membership with an end time only", member, []any{org, w.user(), "pending", nil, when, nil}, chk, "org_members_ended_chk"},
		{"partial: a pending membership with an end reason only", member, []any{org, w.user(), "pending", nil, nil, "left"}, chk, "org_members_ended_chk"},
		{"partial: an active membership with an end reason", member, []any{org, w.user(), "active", when, nil, "left"}, chk, "org_members_ended_chk"},
		{"an ended membership without a reason", member, []any{org, w.user(), "ended", when, when, nil}, chk, "org_members_ended_chk"},
		{"an ended membership without an end time", member, []any{org, w.user(), "ended", when, nil, "left"}, chk, "org_members_ended_chk"},
		{"a pending membership that has joined", member, []any{org, w.user(), "pending", when, nil, nil}, chk, "org_members_active_chk"},
		{"an active membership that has not joined", member, []any{org, w.user(), "active", nil, nil, nil}, chk, "org_members_active_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"a pending request", member, []any{org, w.user(), "pending", nil, nil, nil}, "", ""},
		{"an active membership", member, []any{org, w.user(), "active", when, nil, nil}, "", ""},
		{"an ended membership", member, []any{org, w.user(), "ended", when, when, "left"}, "", ""},
		{"a rejected request (ended without joining)", member, []any{org, w.user(), "ended", nil, when, "rejected"}, "", ""},
	})
}
