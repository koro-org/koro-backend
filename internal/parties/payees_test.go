package parties_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/parties"
	"koro/internal/platform/problem/problemtest"
)

// TestPayee_NaturalPersonPayee_422 (06 H-54): a payee is a legal entity in
// the MVP — an individual or sole proprietorship party is refused as a
// payee (even when it exists past the API), unless the flag lifts H-54.
func TestPayee_NaturalPersonPayee_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sole := parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindPayee, LegalType: parties.LegalSoleProp, Title: "Ahmet Yılmaz",
		VATStatus: parties.VATUnknown, Status: parties.StatusPendingVerification}
	if _, err := e.store.InsertParty(t.Context(), sole); err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	_, err := e.svc.AdminCreatePayee(ctxOf(t, fin), fin, parties.PayeeInput{LegalPartyID: sole.ID, OrgID: org("club_a")})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.natural_person_payee")
	// The payee party itself cannot be created over the API (BE-036).
	in := foundationInput(vknC)
	in.LegalType = parties.LegalSoleProp
	_, err = e.svc.AdminCreateParty(ctxOf(t, fin), fin, in)
	problemtest.WantErr(t, err, parties.ErrNaturalPersonPayee)

	lifted := newEnv(t, naturalPersons)
	if _, err := lifted.store.InsertParty(t.Context(), sole); err != nil {
		t.Fatal(err)
	}
	if _, err := lifted.svc.AdminCreatePayee(ctxOf(t, fin), fin, parties.PayeeInput{LegalPartyID: sole.ID, OrgID: org("club_a")}); err != nil {
		t.Fatalf("with H-54 lifted: %v", err)
	}
}

// TestPayee_AdminLifecycle: create (draft; a club or university; a payee
// party; one open payee per party and org), verify (the party must be
// verified; draft/suspended → active; the default moves), update
// (If-Match; suspend, close, default), all audited.
func TestPayee_AdminLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin, pad := actor(t, "finance_admin"), actor(t, "platform_admin")
	ctxF, ctxP := ctxOf(t, fin), ctxOf(t, pad)
	party := e.foundationOnly(t, vknC)
	in := parties.PayeeInput{LegalPartyID: party.Party.ID, OrgID: org("club_a"), IsDefault: true,
		InvoicingCapability: parties.InvoicingCannotIssue, UniversityProtocolRef: " Protokol 2026/7 "}
	for _, bad := range []struct {
		mut  func(*parties.PayeeInput)
		code string
	}{
		{func(p *parties.PayeeInput) { p.OrgID = org("company_a") }, "validation_failed"},
		{func(p *parties.PayeeInput) { p.OrgID = uuid.NewV7() }, "validation_failed"},
		{func(p *parties.PayeeInput) { p.LegalPartyID = e.buyer(t, vknA).Party.ID }, "validation_failed"},
		{func(p *parties.PayeeInput) { p.LegalPartyID = uuid.NewV7() }, "validation_failed"},
		{func(p *parties.PayeeInput) { p.PayoutMethod = parties.PayoutIyzico }, "validation_failed"},
	} {
		x := in
		bad.mut(&x)
		_, err := e.svc.AdminCreatePayee(ctxF, fin, x)
		if _, code := problemtest.StatusCode(err); code != bad.code {
			t.Fatalf("create %+v: %v, want %s", x, err, bad.code)
		}
	}
	v, err := e.svc.AdminCreatePayee(ctxF, fin, in)
	if err != nil || v.Payee.Status != parties.PayeeDraft || v.Payee.PayoutMethod != parties.PayoutBankEFT ||
		*v.Payee.UniversityProtocolRef != "Protokol 2026/7" || v.Party.Title != "Örnek Eğitim Vakfı" || v.Payee.CreatedBy != fin.UserID {
		t.Fatalf("create = %+v, %v", v, err)
	}
	_, err = e.svc.AdminCreatePayee(ctxF, fin, in)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.payee_exists")
	if _, ok, _ := e.payees().DefaultFor(t.Context(), org("club_a")); ok {
		t.Fatal("a draft payee is nobody's default")
	}
	// The party is pending verification: the payee cannot be activated.
	_, err = e.svc.AdminVerifyPayee(ctxP, pad, v.Payee.ID)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.verification_incomplete")
	e.verify(t, party.Party.ID)
	if v, err = e.svc.AdminVerifyPayee(ctxP, pad, v.Payee.ID); err != nil || v.Payee.Status != parties.PayeeActive ||
		*v.Payee.VerifiedBy != pad.UserID {
		t.Fatalf("verify = %+v, %v", v.Payee, err)
	}
	_, err = e.svc.AdminVerifyPayee(ctxP, pad, v.Payee.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	if def, ok, err := e.payees().DefaultFor(t.Context(), org("club_a")); err != nil || !ok || def.ID != v.Payee.ID {
		t.Fatalf("DefaultFor = %+v %v %v", def, ok, err)
	}

	// A second default payee takes the default over on activation.
	second := e.payee(t, vknD)
	if def, _, _ := e.payees().DefaultFor(t.Context(), org("club_a")); def.ID != second.Payee.ID {
		t.Fatalf("default = %s, want the newer %s", def.ID, second.Payee.ID)
	}
	first, _ := e.store.GetPayee(t.Context(), v.Payee.ID, false)
	if first.IsDefault {
		t.Fatal("the previous default kept its flag")
	}
	// Making the first one default again moves it back (If-Match).
	cur, _ := e.svc.GetOrgPayee(ctxP, pad, org("club_a"), first.ID)
	_, err = e.svc.AdminUpdatePayee(ctxP, pad, first.ID, "", parties.PayeePatch{IsDefault: new(true)})
	problemtest.WantProblem(t, err, http.StatusPreconditionRequired, "precondition_required")
	_, err = e.svc.AdminUpdatePayee(ctxP, pad, first.ID, `"stale"`, parties.PayeePatch{IsDefault: new(true)})
	problemtest.WantProblem(t, err, http.StatusPreconditionFailed, "precondition_failed")
	_, err = e.svc.AdminUpdatePayee(ctxP, pad, first.ID, cur.ETag, parties.PayeePatch{})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.empty_patch")
	upd, err := e.svc.AdminUpdatePayee(ctxP, pad, first.ID, cur.ETag, parties.PayeePatch{IsDefault: new(true)})
	if err != nil || !upd.Payee.IsDefault || upd.ETag == cur.ETag {
		t.Fatalf("make default = %+v, %v", upd, err)
	}
	if def, _, _ := e.payees().DefaultFor(t.Context(), org("club_a")); def.ID != first.ID {
		t.Fatal("the default did not move back")
	}
	ev := e.audit.Of(authz.PartiesPayeeManage)
	if last := ev[len(ev)-1]; !slices.Contains(last.Metadata["previous_default_payee_ids"].([]string), second.Payee.ID.String()) {
		t.Fatalf("update audit = %v", last.Metadata)
	}

	// Suspend: no longer active (payouts stop), no default; verify reactivates.
	susp := parties.PayeeSuspended
	upd, err = e.svc.AdminUpdatePayee(ctxP, pad, first.ID, upd.ETag, parties.PayeePatch{Status: &susp})
	if err != nil || upd.Payee.Status != parties.PayeeSuspended {
		t.Fatalf("suspend = %+v, %v", upd.Payee, err)
	}
	if ok, _ := e.payees().IsActive(t.Context(), first.ID); ok {
		t.Fatal("a suspended payee is active")
	}
	if _, ok, _ := e.payees().DefaultFor(t.Context(), org("club_a")); ok {
		t.Fatal("a suspended default is still the default")
	}
	active := parties.PayeeActive
	_, err = e.svc.AdminUpdatePayee(ctxP, pad, first.ID, upd.ETag, parties.PayeePatch{Status: &active})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	if upd, err = e.svc.AdminVerifyPayee(ctxP, pad, first.ID); err != nil || upd.Payee.Status != parties.PayeeActive {
		t.Fatalf("reactivate = %+v, %v", upd.Payee, err)
	}
	// Close: the end of its validity; nothing changes it afterwards.
	e.clk.Advance(time.Hour)
	closed := parties.PayeeClosed
	upd, err = e.svc.AdminUpdatePayee(ctxP, pad, first.ID, upd.ETag, parties.PayeePatch{Status: &closed})
	if err != nil || upd.Payee.Status != parties.PayeeClosed || upd.Payee.ValidTo == nil {
		t.Fatalf("close = %+v, %v", upd.Payee, err)
	}
	_, err = e.svc.AdminUpdatePayee(ctxP, pad, first.ID, upd.ETag, parties.PayeePatch{IsDefault: new(false)})
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	_, err = e.svc.AdminVerifyPayee(ctxP, pad, first.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	// A closed payee frees its (org, party) pair for a new one.
	if _, err := e.svc.AdminCreatePayee(ctxF, fin, in); err != nil {
		t.Fatalf("new payee after closing: %v", err)
	}
	// Non-staff: 403 before anything is read.
	cla := actor(t, "club_admin")
	_, err = e.svc.AdminCreatePayee(ctxOf(t, cla), cla, in)
	problemtest.WantProblem(t, err, http.StatusForbidden, "forbidden")
}

// TestPayees_Contract pins 01 §4.12 Payees: Get, DefaultFor (active now),
// ApprovedAccount (ErrNoApprovedAccount), IsActive (status and validity).
func TestPayees_Contract(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	ctx := t.Context()
	got, err := e.payees().Get(ctx, py.Payee.ID)
	if err != nil || got.ID != py.Payee.ID || got.OrgID != org("club_a") {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := e.payees().Get(ctx, uuid.NewV7()); !errors.Is(err, parties.ErrNotFound) {
		t.Fatalf("Get of an unknown payee: %v", err)
	}
	if ok, err := e.payees().IsActive(ctx, py.Payee.ID); err != nil || !ok {
		t.Fatalf("IsActive = %v, %v", ok, err)
	}
	if _, err := e.payees().IsActive(ctx, uuid.NewV7()); !errors.Is(err, parties.ErrNotFound) {
		t.Fatalf("IsActive of an unknown payee: %v", err)
	}
	if _, ok, err := e.payees().DefaultFor(ctx, org("uni_a")); ok || err != nil {
		t.Fatalf("DefaultFor of an org without payees: %v %v", ok, err)
	}
	_, err = e.payees().ApprovedAccount(ctx, py.Payee.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.no_approved_account")
	b := e.approved(t, py.Payee.ID, ibanA)
	acc, err := e.payees().ApprovedAccount(ctx, py.Payee.ID)
	if err != nil || acc.ID != b.Account.ID || acc.IBAN != ibanA || acc.HolderName != "ÖRNEK EĞİTİM VAKFI" {
		t.Fatalf("ApprovedAccount = %+v, %v", acc, err)
	}
}

// TestPayee_OrgSurface: club admins and university reps read their org's
// payees (S), a member without the role is 403, another org's payee 404
// (06 H-38); the approved account is masked for everyone here.
func TestPayee_OrgSurface(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	e.approved(t, py.Payee.ID, ibanA)
	cla := actor(t, "club_admin")
	vs, err := e.svc.ListOrgPayees(ctxOf(t, cla), cla, org("club_a"))
	if err != nil || len(vs) != 1 || vs[0].Approved == nil || vs[0].Approved.MaskedIBAN() != "TR33 **** **** **** **** 8413 26" {
		t.Fatalf("list = %+v, %v", vs, err)
	}
	_, err = e.svc.ListOrgPayees(ctxOf(t, actor(t, "club_member")), actor(t, "club_member"), org("club_a"))
	problemtest.WantProblem(t, err, http.StatusForbidden, "forbidden")
	_, err = e.svc.ListOrgPayees(ctxOf(t, actor(t, "student")), actor(t, "student"), org("club_a"))
	problemtest.WantErr(t, err, parties.ErrNotFound)
	_, err = e.svc.ListOrgPayees(ctxOf(t, cla), cla, uuid.NewV7())
	problemtest.WantErr(t, err, parties.ErrNotFound)
	rep := actor(t, "university_rep")
	_, err = e.svc.GetOrgPayee(ctxOf(t, rep), rep, org("uni_a"), py.Payee.ID)
	problemtest.WantErr(t, err, parties.ErrNotFound)
	pad := actor(t, "platform_admin")
	_, err = e.svc.GetOrgPayee(ctxOf(t, pad), pad, org("uni_a"), py.Payee.ID)
	problemtest.WantErr(t, err, parties.ErrNotFound)
	app := newHTTP(t, e, nil)
	rec, out := call(t, app, "club_admin", http.MethodGet, "/api/v1/orgs/"+org("club_a").String()+"/payees/"+py.Payee.ID.String(), nil, nil)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("ETag") == "" || out["approved_account"].(map[string]any)["iban_masked"] != "TR33 **** **** **** **** 8413 26" ||
		out["legal_party"].(map[string]any)["title"] != "Örnek Eğitim Vakfı" {
		t.Fatalf("getOrgPayee:\n%s", rec.Body.String())
	}
}

// TestOwnershipCorrection_H60: a party holds a VKN it does not own; a
// platform admin (not finance) retires it with the reason and the
// evidence, kept as a document of the party; the real owner registers.
func TestOwnershipCorrection_H60(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	wrong := e.buyer(t, vknA) // company_a registered VKN A …
	e.verify(t, wrong.Party.ID)
	pad, fin := actor(t, "platform_admin"), actor(t, "finance_admin")
	evidence := e.files.add(pad.UserID, nil)
	in := parties.OwnershipCorrection{Reason: "Vergi levhası VKN'nin başka bir şirkete ait olduğunu gösteriyor", EvidenceFileID: evidence}
	_, err := e.svc.AdminCorrectOwnership(ctxOf(t, fin), fin, wrong.Party.ID, in)
	problemtest.WantProblem(t, err, http.StatusForbidden, "forbidden")
	cla := actor(t, "club_admin")
	_, err = e.svc.AdminCorrectOwnership(ctxOf(t, cla), cla, wrong.Party.ID, in)
	problemtest.WantProblem(t, err, http.StatusForbidden, "forbidden")
	short := in
	short.Reason = "kısa"
	_, err = e.svc.AdminCorrectOwnership(ctxOf(t, pad), pad, wrong.Party.ID, short)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	foreign := in
	foreign.EvidenceFileID = e.files.add(fin.UserID, nil) // not the caller's upload
	_, err = e.svc.AdminCorrectOwnership(ctxOf(t, pad), pad, wrong.Party.ID, foreign)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	v, err := e.svc.AdminCorrectOwnership(ctxOf(t, pad), pad, wrong.Party.ID, in)
	if err != nil || v.Party.Status != parties.StatusInactive || v.Party.VerifiedBy != nil || len(v.Documents) != 1 ||
		v.Documents[0].Kind != parties.DocOther || v.Documents[0].FileID != evidence {
		t.Fatalf("correction = %+v, %v", v, err)
	}
	ev := e.audit.Of(authz.PartiesLegalPartyVerify)
	last := ev[len(ev)-1]
	if last.Reason != in.Reason || last.Metadata["operation"] != "ownership_correction" || last.Metadata["to"] != "inactive" {
		t.Fatalf("audit = %+v", last)
	}
	if e.files.attached[evidence].Purpose != files.PurposePartyDocument {
		t.Fatal("the evidence is not attached")
	}
	_, err = e.svc.AdminCorrectOwnership(ctxOf(t, pad), pad, wrong.Party.ID, in)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	// The VKN is free again for its owner.
	cma := actor(t, "company_admin")
	if _, err := e.svc.CreateBuyerParty(ctxOf(t, cma), cma, org("company_a"), buyerInput(vknA)); err != nil {
		t.Fatalf("the owner registers: %v", err)
	}
	// The platform party is never corrected (it has no competing owner).
	plat, _, err := e.store.InsertPlatformIfAbsent(t.Context(), parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindPlatform,
		LegalType: parties.LegalCompany, Title: "Koro", VATStatus: parties.VATUnknown, Status: parties.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	in.EvidenceFileID = e.files.add(pad.UserID, nil)
	_, err = e.svc.AdminCorrectOwnership(ctxOf(t, pad), pad, plat.ID, in)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
}

// TestAdminDocuments_T234: the admin surface reads a party with its ETag,
// adds a document to a party no org surface reaches (a foundation without
// a payee; the caller's own party_document upload) and downloads it, every
// download audited.
func TestAdminDocuments_T234(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	party := e.foundationOnly(t, vknC)
	pad, fin := actor(t, "platform_admin"), actor(t, "finance_admin")
	v, err := e.svc.AdminGetParty(ctxOf(t, fin), fin, party.Party.ID)
	if err != nil || v.ETag == "" || !v.FullTaxIDs {
		t.Fatalf("AdminGetParty = %+v, %v", v, err)
	}
	cla := actor(t, "club_admin")
	_, err = e.svc.AdminGetParty(ctxOf(t, cla), cla, party.Party.ID)
	problemtest.WantProblem(t, err, http.StatusForbidden, "forbidden")
	_, err = e.svc.AdminGetParty(ctxOf(t, fin), fin, uuid.NewV7())
	problemtest.WantErr(t, err, parties.ErrNotFound)

	other := e.files.add(fin.UserID, nil)
	_, err = e.svc.AdminUploadDocument(ctxOf(t, pad), pad, party.Party.ID, parties.DocumentInput{Kind: parties.DocFoundationDeed, FileID: other})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	own := e.files.add(pad.UserID, nil)
	d, err := e.svc.AdminUploadDocument(ctxOf(t, pad), pad, party.Party.ID, parties.DocumentInput{Kind: parties.DocFoundationDeed, FileID: own})
	if err != nil || d.UploadedBy != pad.UserID || d.LegalPartyID != party.Party.ID {
		t.Fatalf("AdminUploadDocument = %+v, %v", d, err)
	}
	// The uploader never verifies it; another admin does.
	_, err = e.svc.AdminVerifyDocument(ctxOf(t, pad), pad, d.ID)
	problemtest.WantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	if _, err := e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, d.ID); err != nil {
		t.Fatal(err)
	}
	l, err := e.svc.AdminDownloadDocument(ctxOf(t, fin), fin, d.ID)
	if err != nil || l.URL == "" || !strings.Contains(l.URL, own.String()) {
		t.Fatalf("AdminDownloadDocument = %+v, %v", l, err)
	}
	reads := e.audit.Of(authz.PartiesDocumentRead)
	if len(reads) != 1 || reads[0].Metadata["surface"] != "admin" || *reads[0].ResourceID != d.ID {
		t.Fatalf("download audit = %+v", reads)
	}
	_, err = e.svc.AdminDownloadDocument(ctxOf(t, fin), fin, uuid.NewV7())
	problemtest.WantErr(t, err, parties.ErrNotFound)
}

// TestPrivacy_PayeesAndBankAccounts: the KVKK export lists the payees a
// user created and the bank accounts they submitted (IBAN masked); erasure
// retains them (financial records).
func TestPrivacy_PayeesAndBankAccounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	e.orgSubmit(t, py.Payee.ID, ibanA)
	cla, fin := actor(t, "club_admin"), actor(t, "finance_admin")
	sec, err := e.mod.Privacy.Export(t.Context(), cla.UserID)
	if err != nil || len(sec.Items) != 1 || sec.Items[0]["record"] != "payee_bank_account_submitted" ||
		sec.Items[0]["iban_masked"] != "TR33 **** **** **** **** 8413 26" {
		t.Fatalf("export = %+v, %v", sec, err)
	}
	if strings.Contains(dump(sec), ibanA) {
		t.Fatal("the export holds a full IBAN")
	}
	sec, err = e.mod.Privacy.Export(t.Context(), fin.UserID)
	if err != nil || !slices.ContainsFunc(sec.Items, func(m map[string]any) bool { return m["record"] == "payee_created" }) {
		t.Fatalf("finance export = %+v, %v", sec, err)
	}
	res, err := e.mod.Privacy.Erase(t.Context(), cla.UserID)
	if err != nil || res.Retained != 1 || res.Deleted != 0 || !strings.Contains(res.RetainedReason, "bank accounts") {
		t.Fatalf("erase = %+v, %v", res, err)
	}
}
