package identity_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/privacy"
)

// TestSweeper_StudentDocumentsAndHistory (edu_document, 30 days;
// edu_verification_history, 1 year): unapproved, nothing changes; approved,
// the decided document's file is deleted and its reference cleared (the
// verification stays in force), an ended verification goes with its file.
// Clearing the reference is an update: the history of a rejected row then
// counts from that update (about a month later; conservative).
func TestSweeper_StudentDocumentsAndHistory(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	reviewer := e.activeUser(t, "reviewer@koro.test")
	users := map[string]identity.User{}
	for _, email := range []string{"belge@koro.test", "ret@koro.test", "yeni@koro.test"} {
		users[email] = e.activeUser(t, email)
	}
	ctx := t.Context()
	now := e.clk.Now()
	if got := e.mod.Sweeper.PolicyCodes(); !slices.Equal(got, []string{identity.PolicyEduDocument, identity.PolicyEduVerificationHistory,
		identity.PolicyPrivilegedActionHistory, identity.PolicyRoleHistory}) {
		t.Fatalf("PolicyCodes() = %v", got)
	}
	doc := func(email string, status identity.EduStatus, decided time.Time) identity.EduRecord {
		t.Helper()
		u := users[email]
		file := e.files.add(u.ID, files.PurposeEduDocument, files.ScanClean)
		uni := eduUni
		v := identity.EduRecord{ID: uuid.NewV7(), UserID: u.ID, Method: identity.EduMethodDocument, UniversityID: &uni,
			DocumentFileID: &file, Status: status, ReviewedBy: &reviewer.ID}
		if decided.Before(now.AddDate(-1, 0, 0)) {
			v.DocumentFileID = nil // purged long ago, the row ended over a year ago
		}
		if status == identity.EduVerified {
			exp := decided.AddDate(1, 0, 0)
			v.VerifiedAt, v.ExpiresAt = &decided, &exp
		}
		// Decided then: the store stamps the row at decided (one clock, BE-069).
		if err := e.store.SeedAt(decided, func() error { return e.store.InsertEduVerification(ctx, v) }); err != nil {
			t.Fatal(err)
		}
		return v
	}
	inForce := doc("belge@koro.test", identity.EduVerified, now.AddDate(0, 0, -31))
	rejectedOld := doc("ret@koro.test", identity.EduRejected, now.AddDate(-2, 0, 0))
	fresh := doc("yeni@koro.test", identity.EduVerified, now.AddDate(0, 0, -5))

	docPolicy := privacy.RetentionPolicy{Code: identity.PolicyEduDocument, Retention: 30 * 24 * time.Hour}
	histPolicy := privacy.RetentionPolicy{Code: identity.PolicyEduVerificationHistory, Retention: now.Sub(now.AddDate(-1, 0, 0))}
	for _, p := range []privacy.RetentionPolicy{docPolicy, histPolicy} {
		res, err := e.mod.Sweeper.Sweep(ctx, p, now)
		if err != nil || res.Count == 0 {
			t.Fatalf("dry run %s = %+v, %v", p.Code, res, err)
		}
	}
	if len(e.files.removed) != 0 {
		t.Fatalf("a dry run deleted files: %v", e.files.removed)
	}

	docPolicy.LegalApproved, histPolicy.LegalApproved = true, true
	res, err := e.mod.Sweeper.Sweep(ctx, docPolicy, now)
	if err != nil || res.Count != 1 || res.Method != privacy.MethodFileDelete || res.Table != "files:edu_document" {
		t.Fatalf("edu_document sweep = %+v, %v; want the document decided over 30 days ago", res, err)
	}
	got, _ := e.store.EduVerification(ctx, inForce.ID)
	if got.DocumentFileID != nil || got.Status != identity.EduVerified {
		t.Fatalf("verification after the document purge = %+v (must stay verified, without its file)", got)
	}
	if !slices.Equal(e.files.removed, []uuid.UUID{*inForce.DocumentFileID}) {
		t.Fatalf("files removed = %v", e.files.removed)
	}
	res, err = e.mod.Sweeper.Sweep(ctx, histPolicy, now)
	if err != nil || res.Count != 1 {
		t.Fatalf("history sweep = %+v, %v; want the rejected row", res, err)
	}
	if _, err := e.store.EduVerification(ctx, rejectedOld.ID); err == nil {
		t.Fatal("the rejected verification of 2 years ago survived")
	}
	for _, id := range []uuid.UUID{inForce.ID, fresh.ID} {
		if _, err := e.store.EduVerification(ctx, id); err != nil {
			t.Fatalf("a verification in force was purged: %v", err)
		}
	}
}
