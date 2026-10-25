package parties_test

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/parties"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

func ids(vs []parties.PartyView) []uuid.UUID {
	out := make([]uuid.UUID, len(vs))
	for i, v := range vs {
		out[i] = v.Party.ID
	}
	return out
}

// TestOrgScope_K21_PayeeSideOnly: a club or university sees the legal side
// of its payees (scope payee: a payees row, not closed), never another
// org's party (06 H-38: the org comes from the rows); a company sees its
// own buyer parties; the platform party belongs to no org.
func TestOrgScope_K21_PayeeSideOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	buyer := e.buyer(t, vknA)
	found := e.foundation(t, vknB) // payee of club_a
	fin := actor(t, "finance_admin")
	uni := foundationInput(vknC)
	uni.LegalType, uni.OrgID = parties.LegalUniversity, ptr(org("uni_a"))
	uniOwn, err := e.svc.AdminCreateParty(ctxOf(t, fin), fin, uni)
	if err != nil {
		t.Fatal(err)
	}

	ca, cm, cma, cmm, ur := actor(t, "club_admin"), actor(t, "club_member"), actor(t, "company_admin"), actor(t, "company_member"), actor(t, "university_rep")
	list := func(a authz.Actor, o string) ([]uuid.UUID, error) {
		vs, err := e.svc.ListOrgParties(ctxOf(t, a), a, org(o))
		return ids(vs), err
	}
	if got, err := list(ca, "club_a"); err != nil || !slices.Equal(got, []uuid.UUID{found.Party.ID}) {
		t.Fatalf("club_admin list: %v %v", got, err)
	}
	if got, err := list(cmm, "company_a"); err != nil || !slices.Equal(got, []uuid.UUID{buyer.Party.ID}) {
		t.Fatalf("company_member list: %v %v", got, err)
	}
	if _, err := list(cm, "club_a"); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("club_member list: %v", err)
	}
	if _, err := list(ca, "company_a"); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("club_admin on company_a: %v", err)
	}
	// k21: the university's own party is not its payee side until a payee links it.
	if got, err := list(ur, "uni_a"); err != nil || len(got) != 0 {
		t.Fatalf("university_rep list without payee: %v %v", got, err)
	}
	if got, err := list(fin, "uni_a"); err != nil || !slices.Equal(got, []uuid.UUID{uniOwn.Party.ID}) {
		t.Fatalf("finance on uni_a: %v %v", got, err)
	}
	_, err = e.svc.GetOrgParty(ctxOf(t, ur), ur, org("uni_a"), uniOwn.Party.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	e.store.LinkPayee(uuid.NewV7(), org("uni_a"), uniOwn.Party.ID, "pending_verification")
	if got, err := list(ur, "uni_a"); err != nil || !slices.Equal(got, []uuid.UUID{uniOwn.Party.ID}) {
		t.Fatalf("university_rep list with payee: %v %v", got, err)
	}

	// IDOR: another org's party under this org's path is 404 for everyone
	// who may read the surface; without a role it is 403 before the lookup.
	for _, tc := range []struct {
		a    authz.Actor
		org  string
		id   uuid.UUID
		want error
	}{
		{ca, "club_a", buyer.Party.ID, authz.ErrNotFound},
		{fin, "club_a", buyer.Party.ID, authz.ErrNotFound},
		{cma, "company_a", found.Party.ID, authz.ErrNotFound},
		{cm, "club_a", buyer.Party.ID, authz.ErrForbidden},
		{cm, "club_a", found.Party.ID, authz.ErrForbidden},
		{actor(t, "other_club_admin"), "club_b", found.Party.ID, authz.ErrNotFound},
		{actor(t, "other_club_admin"), "club_a", found.Party.ID, authz.ErrNotFound},
		{ca, "club_a", uuid.Nil(), authz.ErrNotFound},
		{ca, "unknown_org", found.Party.ID, authz.ErrNotFound},
	} {
		_, err := e.svc.GetOrgParty(ctxOf(t, tc.a), tc.a, org(tc.org), tc.id)
		if !errors.Is(err, tc.want) {
			t.Errorf("GetOrgParty(%s, %s): %v, want %v", tc.org, tc.id, err, tc.want)
		}
	}
	// A closed payee row relates nothing any more.
	closed := e.foundation(t, vknD)
	e.store.LinkPayee(uuid.NewV7(), org("club_b"), closed.Party.ID, "closed")
	if _, err := e.svc.GetOrgParty(ctxOf(t, actor(t, "other_club_admin")), actor(t, "other_club_admin"), org("club_b"), closed.Party.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("closed payee: %v", err)
	}
	// The platform party is on no org surface.
	sd, _ := parties.NewSeeder(parties.SeederDeps{Store: e.store, Tx: dbNop(), Audit: e.audit, Clock: e.clk, Log: discard()})
	p, _, err := sd.EnsurePlatform(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetOrgParty(ctxOf(t, fin), fin, org("platform_org"), p.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("platform party on the platform org: %v", err)
	}
}

// TestUploadDocument_FilesPipeline: a document is a party_document upload
// of the caller or of the org, attached in the same transaction and
// audited; others are 422; the H-38 parent rule holds.
func TestUploadDocument_FilesPipeline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	found := e.foundation(t, vknB)
	ca := actor(t, "club_admin")
	club := org("club_a")
	in := func(f uuid.UUID) parties.DocumentInput {
		return parties.DocumentInput{Kind: parties.DocFoundationDeed, FileID: f}
	}
	// The caller's own upload without an org, and an org upload of another member.
	own := e.files.add(ca.UserID, nil)
	byOrg := e.files.add(authztest.ID("someone_else"), &club)
	for _, f := range []uuid.UUID{own, byOrg} {
		d, err := e.svc.UploadDocument(ctxOf(t, ca), ca, club, found.Party.ID, in(f))
		if err != nil || d.UploadedBy != ca.UserID || d.LegalPartyID != found.Party.ID || d.VerifiedAt != nil {
			t.Fatalf("upload %s: %+v %v", f, d, err)
		}
	}
	if ev := e.audit.Of(authz.PartiesDocumentUpload); len(ev) != 2 || ev[0].OrgID == nil || *ev[0].OrgID != club {
		t.Fatalf("upload audit = %+v", ev)
	}
	// Someone else's file, another org's file, another purpose, a missing file: 422.
	companyA := org("company_a")
	cv := e.files.add(ca.UserID, nil)
	e.files.set(cv, func(m *files.FileMeta) { m.Purpose = files.PurposeCV })
	otherOrg := e.files.add(authztest.ID("stranger"), &companyA)
	for _, f := range []uuid.UUID{e.files.add(authztest.ID("stranger"), nil), otherOrg, cv, uuid.NewV7()} {
		_, err := e.svc.UploadDocument(ctxOf(t, ca), ca, club, found.Party.ID, in(f))
		problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	}
	// A file attached to one document cannot back another (files: 409).
	_, err := e.svc.UploadDocument(ctxOf(t, ca), ca, club, found.Party.ID, in(own))
	problemtest.WantProblem(t, err, http.StatusConflict, "files.already_attached")
	// A bad kind is 422; another org's party (H-38) is 404; no role is 403.
	_, err = e.svc.UploadDocument(ctxOf(t, ca), ca, club, found.Party.ID, parties.DocumentInput{Kind: "passport", FileID: e.files.add(ca.UserID, nil)})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	buyer := e.buyer(t, vknA)
	_, err = e.svc.UploadDocument(ctxOf(t, ca), ca, club, buyer.Party.ID, in(e.files.add(ca.UserID, nil)))
	problemtest.WantErr(t, err, authz.ErrNotFound)
	cm := actor(t, "club_member")
	_, err = e.svc.UploadDocument(ctxOf(t, cm), cm, club, found.Party.ID, in(e.files.add(cm.UserID, nil)))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	// The documents are listed with the party.
	v, err := e.svc.GetOrgParty(ctxOf(t, ca), ca, club, found.Party.ID)
	if err != nil || len(v.Documents) != 2 {
		t.Fatalf("documents of the party: %+v %v", v.Documents, err)
	}
	// Without the files module the document operations fail closed.
	nf := newEnv(t, func(d *parties.Deps) { d.Files = nil })
	f2 := nf.foundation(t, vknB)
	_, err = nf.svc.UploadDocument(ctxOf(t, ca), ca, club, f2.Party.ID, in(uuid.NewV7()))
	problemtest.WantErr(t, err, problem.ErrUnavailable)
}

// TestVerifyDocument_FourEyesAndCleanFile: finance or platform admins verify
// a document they did not upload, once, and only a file that could be
// looked at.
func TestVerifyDocument_FourEyesAndCleanFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	found := e.foundation(t, vknB)
	club := org("club_a")
	pad, fin := actor(t, "platform_admin"), actor(t, "finance_admin")
	f := e.files.add(pad.UserID, &club)
	d, err := e.svc.UploadDocument(ctxOf(t, pad), pad, club, found.Party.ID, parties.DocumentInput{Kind: parties.DocTaxCertificate, FileID: f})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminVerifyDocument(ctxOf(t, pad), pad, d.ID)
	problemtest.WantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	ca := actor(t, "club_admin")
	_, err = e.svc.AdminVerifyDocument(ctxOf(t, ca), ca, d.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, uuid.NewV7())
	problemtest.WantErr(t, err, parties.ErrNotFound)
	e.files.set(f, func(m *files.FileMeta) { m.ScanStatus = files.ScanInfected })
	_, err = e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, d.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "files.not_clean")
	e.files.set(f, func(m *files.FileMeta) { m.Status, m.ScanStatus = files.StatusRejected, files.ScanClean })
	_, err = e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, d.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "files.rejected")
	e.files.set(f, func(m *files.FileMeta) { m.Status, m.ScanStatus = files.StatusActive, files.ScanSkipped })
	got, err := e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, d.ID)
	if err != nil || got.VerifiedAt == nil || got.VerifiedBy == nil || *got.VerifiedBy != fin.UserID {
		t.Fatalf("verify: %+v %v", got, err)
	}
	ev := e.audit.Of(authz.PartiesDocumentVerify)
	if len(ev) != 1 || ev[0].ResourceID == nil || *ev[0].ResourceID != d.ID || ev[0].Metadata["legal_party_id"] != found.Party.ID.String() {
		t.Fatalf("verify audit = %+v", ev)
	}
	_, err = e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, d.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
}

// TestAdminVerifyParty_StateMachine: draft, pending or rejected → verified
// with a tax ID and an address; draft or pending → rejected; anything
// else 409; the decision is idempotent under an Idempotency-Key.
func TestAdminVerifyParty_StateMachine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	in := foundationInput(vknB)
	in.Address = nil
	bare, err := e.svc.AdminCreateParty(ctxOf(t, fin), fin, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminVerifyParty(ctxOf(t, fin), fin, bare.Party.ID, parties.Decision{Decision: parties.StatusVerified})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.verification_incomplete")
	_, err = e.svc.AdminVerifyParty(ctxOf(t, fin), fin, bare.Party.ID, parties.Decision{Decision: "inactive"})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	cma := actor(t, "company_admin")
	_, err = e.svc.AdminVerifyParty(ctxOf(t, cma), cma, bare.Party.ID, parties.Decision{Decision: parties.StatusVerified})
	problemtest.WantErr(t, err, authz.ErrForbidden)

	p := e.buyer(t, vknA)
	ctx := idemCtx(t, fin, "verify-key-1")
	v1, err := e.svc.AdminVerifyParty(ctx, fin, p.Party.ID, parties.Decision{Decision: parties.StatusVerified, Note: "Levha uygun"})
	if err != nil || v1.Party.Status != parties.StatusVerified {
		t.Fatalf("verify: %+v %v", v1.Party, err)
	}
	v2, err := e.svc.AdminVerifyParty(ctx, fin, p.Party.ID, parties.Decision{Decision: parties.StatusVerified, Note: "Levha uygun"})
	if err != nil || v2.Party.Status != parties.StatusVerified || len(e.audit.Of(authz.PartiesLegalPartyVerify)) != 1 {
		t.Fatalf("replay: %+v %v (%d events)", v2.Party, err, len(e.audit.Of(authz.PartiesLegalPartyVerify)))
	}
	if ev := e.audit.Of(authz.PartiesLegalPartyVerify)[0]; ev.Reason != "Levha uygun" || ev.Metadata["to"] != "verified" {
		t.Fatalf("verify audit = %+v", ev)
	}
	_, err = e.svc.AdminVerifyParty(ctxOf(t, fin), fin, p.Party.ID, parties.Decision{Decision: parties.StatusVerified})
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	_, err = e.svc.AdminVerifyParty(ctxOf(t, fin), fin, p.Party.ID, parties.Decision{Decision: parties.StatusRejected})
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	_, err = e.svc.AdminVerifyParty(ctxOf(t, fin), fin, uuid.NewV7(), parties.Decision{Decision: parties.StatusVerified})
	problemtest.WantErr(t, err, parties.ErrNotFound)
}

// TestAdminListParties_FiltersAndPages: filters by kind, status and q (title
// part, Turkish-insensitive, or an exact VKN), newest first, keyset pages.
func TestAdminListParties_FiltersAndPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.buyer(t, vknA)
	b := e.foundation(t, vknB)
	c := e.foundation(t, vknC)
	fin := actor(t, "finance_admin")
	q := func(aq parties.AdminQuery) []uuid.UUID {
		t.Helper()
		p, err := e.svc.AdminListParties(ctxOf(t, fin), fin, aq)
		if err != nil {
			t.Fatal(err)
		}
		return ids(p.Items)
	}
	if got := q(parties.AdminQuery{}); !slices.Equal(got, []uuid.UUID{c.Party.ID, b.Party.ID, a.Party.ID}) {
		t.Fatalf("all: %v", got)
	}
	if got := q(parties.AdminQuery{Kind: parties.KindPayee}); !slices.Equal(got, []uuid.UUID{c.Party.ID, b.Party.ID}) {
		t.Fatalf("payees: %v", got)
	}
	if got := q(parties.AdminQuery{Q: "EĞİTİM vakfı"}); len(got) != 2 {
		t.Fatalf("title search: %v", got)
	}
	if got := q(parties.AdminQuery{Q: vknA}); !slices.Equal(got, []uuid.UUID{a.Party.ID}) {
		t.Fatalf("VKN search: %v", got)
	}
	e.verify(t, a.Party.ID)
	if got := q(parties.AdminQuery{Status: parties.StatusVerified}); !slices.Equal(got, []uuid.UUID{a.Party.ID}) {
		t.Fatalf("verified: %v", got)
	}
	p, err := e.svc.AdminListParties(ctxOf(t, fin), fin, parties.AdminQuery{Limit: 2})
	if err != nil || len(p.Items) != 2 || p.Next == nil {
		t.Fatalf("first page: %+v %v", p, err)
	}
	p, err = e.svc.AdminListParties(ctxOf(t, fin), fin, parties.AdminQuery{Limit: 2, After: p.Next})
	if err != nil || len(p.Items) != 1 || p.Next != nil || p.Items[0].Party.ID != a.Party.ID {
		t.Fatalf("second page: %+v %v", p, err)
	}
	for _, bad := range []parties.AdminQuery{{Kind: "x"}, {Status: "x"}, {Q: "a"}} {
		_, err := e.svc.AdminListParties(ctxOf(t, fin), fin, bad)
		problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	}
	cma := actor(t, "company_admin")
	_, err = e.svc.AdminListParties(ctxOf(t, cma), cma, parties.AdminQuery{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

// TestPrivacy_ExportAndRetain: the KVKK provider exports the parties and
// documents a user entered and retains everything on erasure (financial
// records, 10 years).
func TestPrivacy_ExportAndRetain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cma := actor(t, "company_admin")
	p := e.buyer(t, vknA)
	co := org("company_a")
	if _, err := e.svc.UploadDocument(ctxOf(t, cma), cma, co, p.Party.ID, parties.DocumentInput{
		Kind: parties.DocTaxCertificate, FileID: e.files.add(cma.UserID, &co)}); err != nil {
		t.Fatal(err)
	}
	pv := e.mod.Privacy
	if pv.Name() != "parties" {
		t.Fatalf("provider name %q", pv.Name())
	}
	sec, err := pv.Export(t.Context(), cma.UserID)
	if err != nil || sec.Module != "parties" || len(sec.Items) != 2 {
		t.Fatalf("export: %+v %v", sec, err)
	}
	if sec.Items[0]["record"] != "legal_party_created" || sec.Items[0]["title"] != "Örnek Yazılım A.Ş." ||
		sec.Items[1]["record"] != "legal_party_document_uploaded" {
		t.Fatalf("export items: %+v", sec.Items)
	}
	noDigits(t, "export", dump(sec), vknA, mersisA)
	for range 2 {
		res, err := pv.Erase(t.Context(), cma.UserID)
		if err != nil || res.Deleted != 0 || res.Anonymized != 0 || res.Retained != 2 || res.RetainedReason == "" {
			t.Fatalf("erase: %+v %v", res, err)
		}
	}
	if _, err := e.store.GetParty(t.Context(), p.Party.ID); err != nil {
		t.Fatalf("erasure removed the party: %v", err)
	}
	res, err := pv.Erase(t.Context(), uuid.NewV7())
	if err != nil || res.Retained != 0 || res.RetainedReason != "" {
		t.Fatalf("erase of a stranger: %+v %v", res, err)
	}
}
