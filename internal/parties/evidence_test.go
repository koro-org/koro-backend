package parties_test

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/files/filestest"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/parties"
	"koro/internal/platform/problem"
)

// infected marks an upload rejected by the scan after its upload.
func (e *env) infected(id uuid.UUID) {
	e.files.set(id, func(m *files.FileMeta) { m.ScanStatus = files.ScanInfected })
}

// rejectedNotice delivers file.rejected of file (purpose, attached to
// entity) to parties' subscription.
func (e *env) rejectedNotice(t *testing.T, file uuid.UUID, purpose files.Purpose, entity uuid.UUID) {
	t.Helper()
	if err := filestest.Reject(t, e.mod.Subscribe, parties.NotifyRejectedEvidenceTaskName, file, purpose, files.RejectInfected,
		"parties", entity); err != nil {
		t.Fatal(err)
	}
}

// replacedRecord returns the replace_rejected_file audit record of act.
func (e *env) replacedRecord(t *testing.T, act authz.Action, old, next uuid.UUID) map[string]any {
	t.Helper()
	for _, ev := range e.audit.Of(act) {
		if ev.Metadata["operation"] == "replace_rejected_file" {
			if ev.Metadata["replaced_file_id"] != old.String() || ev.Metadata["file_id"] != next.String() {
				t.Fatalf("replacement audit = %+v", ev.Metadata)
			}
			return ev.Metadata
		}
	}
	t.Fatalf("no replace_rejected_file record of %s", act)
	return nil
}

// TestReplaceEvidence_DocumentFile (BE-154b): a verified buyer document
// whose file the scan rejects later shows rejected; only a rejected file
// is replaced, with an upload of the caller or of the company; the
// replacer becomes the uploader, the verification is reset and the
// replacer may not verify the new file (06 H-60); the company's admins and
// finance are told; a late rejection of the replaced file is silent.
func TestReplaceEvidence_DocumentFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	party := e.buyer(t, "1234567890").Party
	cma, fin, fin2 := actor(t, "company_admin"), actor(t, "finance_admin"), actor(t, "finance_admin_2")
	company := org("company_a")
	old := e.files.add(cma.UserID, &company)
	d, err := e.svc.UploadDocument(ctxOf(t, cma), cma, company, party.ID, parties.DocumentInput{Kind: parties.DocTaxCertificate, FileID: old})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, d.ID); err != nil {
		t.Fatal(err)
	}
	replace := func(who authz.Actor, file uuid.UUID) (parties.DocumentView, error) {
		return e.svc.ReplaceDocumentFile(ctxOf(t, who), who, company, party.ID, d.ID, file)
	}
	next := e.files.add(cma.UserID, &company)
	if _, err := replace(cma, next); !errors.Is(err, files.ErrNotRejected) {
		t.Fatalf("clean file: %v, want 409 files.not_rejected", err)
	}
	e.infected(old)
	v, err := e.svc.GetOrgParty(ctxOf(t, cma), cma, company, party.ID)
	if err != nil || len(v.Documents) != 1 || files.StateOf(v.FileStates, &v.Documents[0].FileID) != files.StateRejected {
		t.Fatalf("party view = %+v, %v; want the document rejected", v, err)
	}
	e.rejectedNotice(t, old, files.PurposePartyDocument, d.ID)
	if got := recipients(e.sender.all(), "parties.document_rejected", notify.ChannelInApp); !slices.Contains(got, cma.UserID) ||
		!slices.Contains(got, fin.UserID) || !slices.Contains(got, fin2.UserID) || slices.Contains(got, actor(t, "company_member").UserID) {
		t.Fatalf("notice to %v, want the company's admins and the finance admins", got)
	}
	for _, m := range e.sender.all() {
		if m.Template != "parties.document_rejected" {
			continue
		}
		want := "/orgs/" + company.String() + "/legal-parties/" + party.ID.String() // the company's panel (BE-159)
		if *m.UserID == fin.UserID || *m.UserID == fin2.UserID {
			want = "/admin/legal-parties/" + party.ID.String() // finance's party screen
		}
		if m.LinkPath.String() != want {
			t.Errorf("notice to %s links %q, want %q", m.UserID, m.LinkPath, want)
		}
	}
	for persona, want := range map[string]error{"company_member": authz.ErrForbidden, "club_admin": authz.ErrNotFound,
		"support": authz.ErrForbidden} {
		if _, err := replace(actor(t, persona), next); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", persona, err, want)
		}
	}
	other := org("company_b")
	if _, err := replace(cma, e.files.add(actor(t, "other_company_member").UserID, &other)); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("another company's upload: %v, want 422", err)
	}
	got, err := replace(fin2, e.files.add(fin2.UserID, nil))
	if err != nil || got.UploadedBy != fin2.UserID || got.VerifiedAt != nil || got.VerifiedBy != nil || got.FileState != files.StateClean {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	meta := e.replacedRecord(t, authz.PartiesDocumentUpload, old, got.FileID)
	if meta["verification_reset"] != true {
		t.Fatalf("audit %+v, want verification_reset", meta)
	}
	if _, err := e.svc.AdminVerifyDocument(ctxOf(t, fin2), fin2, d.ID); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("the replacer verifies: %v, want 403 four eyes", err)
	}
	if _, err := e.svc.AdminVerifyDocument(ctxOf(t, fin), fin, d.ID); err != nil {
		t.Fatalf("another finance admin verifies: %v", err)
	}
	before := len(e.sender.all())
	e.rejectedNotice(t, old, files.PurposePartyDocument, d.ID)
	if len(e.sender.all()) != before {
		t.Fatal("a late rejection of the replaced file sent a notice")
	}
}

// TestReplaceEvidence_AdminDocumentFile: the admin surface replaces with
// the caller's own upload only; an unknown document is 404.
func TestReplaceEvidence_AdminDocumentFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	party := e.foundationOnly(t, "1111111114").Party
	pad, fin := actor(t, "platform_admin"), actor(t, "finance_admin")
	old := e.files.add(pad.UserID, nil)
	d, err := e.svc.AdminUploadDocument(ctxOf(t, pad), pad, party.ID, parties.DocumentInput{Kind: parties.DocFoundationDeed, FileID: old})
	if err != nil {
		t.Fatal(err)
	}
	e.infected(old)
	cla := actor(t, "club_admin")
	if _, err := e.svc.AdminReplaceDocumentFile(ctxOf(t, cla), cla, d.ID, e.files.add(cla.UserID, nil)); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("club admin on the admin surface: %v, want 403", err)
	}
	if _, err := e.svc.AdminReplaceDocumentFile(ctxOf(t, fin), fin, d.ID, e.files.add(pad.UserID, nil)); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("another admin's upload: %v, want 422", err)
	}
	if _, err := e.svc.AdminReplaceDocumentFile(ctxOf(t, fin), fin, uuid.NewV7(), e.files.add(fin.UserID, nil)); !errors.Is(err, parties.ErrNotFound) {
		t.Fatalf("unknown document: %v, want 404", err)
	}
	next := e.files.add(fin.UserID, nil)
	got, err := e.svc.AdminReplaceDocumentFile(ctxOf(t, fin), fin, d.ID, next)
	if err != nil || got.FileID != next || got.UploadedBy != fin.UserID {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	if meta := e.replacedRecord(t, authz.PartiesDocumentUpload, old, next); meta["surface"] != "admin" {
		t.Fatalf("audit %+v, want surface admin", meta)
	}
	// Only finance hears of a payee party's document (no org of its own).
	e.infected(next)
	e.rejectedNotice(t, next, files.PurposePartyDocument, d.ID)
	if got := recipients(e.sender.all(), "parties.document_rejected", notify.ChannelInApp); len(got) != 2 {
		t.Fatalf("notice to %v, want the two finance admins", got)
	}
	for _, m := range e.sender.all() {
		if m.Template == "parties.document_rejected" && m.LinkPath.String() != "/admin/legal-parties/"+party.ID.String() {
			t.Errorf("notice links %q, want finance's party screen", m.LinkPath)
		}
	}
}

// TestReplaceEvidence_BankAccountEvidence: an org submission waiting for
// the second eye whose evidence the scan rejects goes back to the first eye
// when the evidence is replaced (R; the submit cell); the IBAN and the
// submitter stay; whoever uploaded the evidence is no eye; an approved
// account is submitted again (409); finance and the payee org's admins
// are told.
func TestReplaceEvidence_BankAccountEvidence(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, "1111111114")
	b := e.orgSubmit(t, py.Payee.ID, ibanA)
	if b.EvidenceState != files.StateClean || b.EvidenceUploadedBy == nil || *b.EvidenceUploadedBy != b.Account.SubmittedBy {
		t.Fatalf("view state = %q, uploader %v; want clean, the submitter", b.EvidenceState, b.EvidenceUploadedBy)
	}
	if _, err := e.verifyAccount(t, "finance_admin", b.Account.ID); err != nil {
		t.Fatal(err)
	}
	old := b.Account.EvidenceFileID
	e.infected(old)
	e.rejectedNotice(t, old, files.PurposeBankAccountEvidence, b.Account.ID)
	got := recipients(e.sender.all(), "parties.bank_account_evidence_rejected", notify.ChannelInApp)
	if len(got) != 3 {
		t.Fatalf("notice to %v, want club_admin and the two finance admins", got)
	}
	cla := actor(t, "club_admin")
	club := org("club_a")
	replace := func(who authz.Actor, file uuid.UUID, p identity.ReauthProof) (parties.BankAccountView, error) {
		return e.svc.ReplaceBankAccountEvidence(ctxOf(t, who), who, club, py.Payee.ID, b.Account.ID, file, p)
	}
	next := e.files.addPurpose(files.PurposeBankAccountEvidence, actor(t, "finance_admin_2").UserID, &club)
	if _, err := replace(cla, next, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("no proof: %v, want 403 reauth_required", err)
	}
	if _, err := replace(actor(t, "club_member"), next, proof()); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("club member: %v, want 403", err)
	}
	if _, err := replace(cla, e.files.add(cla.UserID, &club), proof()); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("a party_document upload: %v, want 422", err)
	}
	if _, err := e.svc.ReplaceBankAccountEvidence(ctxOf(t, cla), cla, club, py.Payee.ID, uuid.NewV7(), next, proof()); !errors.Is(err, parties.ErrNotFound) {
		t.Fatalf("unknown account: %v, want 404", err)
	}
	v, err := replace(cla, next, proof())
	if err != nil || v.Account.EvidenceFileID != next || v.Account.Status != parties.AccountPendingVerification ||
		v.Account.VerifiedBy != nil || v.Account.IBAN != b.Account.IBAN || v.Account.SubmittedBy != b.Account.SubmittedBy {
		t.Fatalf("replace = %+v, %v", v.Account, err)
	}
	if v.EvidenceUploadedBy == nil || *v.EvidenceUploadedBy != actor(t, "finance_admin_2").UserID {
		t.Fatalf("view uploader = %v, want the new evidence's uploader (BE-159)", v.EvidenceUploadedBy)
	}
	e.replacedRecord(t, authz.PartiesBankAccountSubmit, old, next)
	// finance_admin_2 uploaded the new evidence: no eye of this account.
	if _, err := e.verifyAccount(t, "finance_admin_2", b.Account.ID); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("the evidence's uploader verifies: %v, want 403 four eyes", err)
	}
	if _, err := e.verifyAccount(t, "finance_admin", b.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.approveAccount(t, "finance_admin_2", b.Account.ID); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("the evidence's uploader approves: %v, want 403 four eyes", err)
	}
	a, err := e.approveAccount(t, "platform_admin", b.Account.ID)
	if err != nil || a.Account.Status != parties.AccountApproved {
		t.Fatalf("approve = %+v, %v", a.Account, err)
	}
	e.infected(next)
	if _, err := replace(cla, e.files.addPurpose(files.PurposeBankAccountEvidence, cla.UserID, &club), proof()); !errors.Is(err, parties.ErrInvalidState) {
		t.Fatalf("approved account: %v, want 409", err)
	}
}

// TestReplaceEvidence_AdminSubmissionEvidence: an org role cannot replace
// the evidence of an admin submission (its submitter vouched for it as the
// first eye); finance can, and the account keeps waiting for approval.
func TestReplaceEvidence_AdminSubmissionEvidence(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, "1111111114")
	pad := actor(t, "platform_admin")
	b, err := e.svc.SubmitPayeeBankAccount(ctxOf(t, pad), pad, org("club_a"), py.Payee.ID, e.accountInput(t, ibanB, "platform_admin"), proof())
	if err != nil || b.Account.Status != parties.AccountPendingApproval {
		t.Fatalf("admin submission = %+v, %v", b.Account, err)
	}
	e.infected(b.Account.EvidenceFileID)
	club := org("club_a")
	cla, fin := actor(t, "club_admin"), actor(t, "finance_admin")
	if _, err := e.svc.ReplaceBankAccountEvidence(ctxOf(t, cla), cla, club, py.Payee.ID, b.Account.ID,
		e.files.addPurpose(files.PurposeBankAccountEvidence, cla.UserID, &club), proof()); !errors.Is(err, parties.ErrInvalidState) {
		t.Fatalf("org role on an admin submission: %v, want 409", err)
	}
	next := e.files.addPurpose(files.PurposeBankAccountEvidence, fin.UserID, &club)
	v, err := e.svc.ReplaceBankAccountEvidence(ctxOf(t, fin), fin, club, py.Payee.ID, b.Account.ID, next, proof())
	if err != nil || v.Account.Status != parties.AccountPendingApproval || v.Account.EvidenceFileID != next {
		t.Fatalf("replace = %+v, %v", v.Account, err)
	}
	if _, err := e.approveAccount(t, "finance_admin", b.Account.ID); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("the evidence's uploader approves: %v, want 403", err)
	}
	if _, err := e.approveAccount(t, "finance_admin_2", b.Account.ID); err != nil {
		t.Fatal(err)
	}
}

// TestReplaceEvidence_HTTPPathParameters (BE-154b): the replace operations
// of the org surface reach the service with every path parameter filled
// (org, payee, bank account; org, party, document): an input that embedded
// an unexported path struct registered without org_id and payee_id and
// answered 404 on every call. A malformed ID of each segment is 404 too.
func TestReplaceEvidence_HTTPPathParameters(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e, nil)
	py := e.payee(t, "1111111114")
	b := e.orgSubmit(t, py.Payee.ID, ibanA)
	e.infected(b.Account.EvidenceFileID)
	club := org("club_a")
	next := e.files.addPurpose(files.PurposeBankAccountEvidence, actor(t, "club_admin").UserID, &club)
	accountPath := func(o, payee, account string) string {
		return "/api/v1/orgs/" + o + "/payees/" + payee + "/bank-accounts/" + account + "/replace-evidence"
	}
	body := map[string]any{"evidence_file_id": next.String(), "reauth": map[string]any{"password": reauthPassword}}
	for name, path := range map[string]string{
		"org":     accountPath("x", py.Payee.ID.String(), b.Account.ID.String()),
		"payee":   accountPath(club.String(), "x", b.Account.ID.String()),
		"account": accountPath(club.String(), py.Payee.ID.String(), "x"),
	} {
		rec, _ := call(t, app, "club_admin", http.MethodPost, path, body, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("malformed %s: %d, want 404:\n%s", name, rec.Code, rec.Body.String())
		}
	}
	rec, out := call(t, app, "club_admin", http.MethodPost, accountPath(club.String(), py.Payee.ID.String(), b.Account.ID.String()), body, nil)
	wantStatus(t, rec, http.StatusOK)
	if out["bank_account_id"] != b.Account.ID.String() || out["payee_id"] != py.Payee.ID.String() || out["evidence_file_state"] != "clean" {
		t.Fatalf("answer:\n%s", rec.Body.String())
	}
	if got, err := e.store.GetBankAccount(t.Context(), b.Account.ID, false); err != nil || got.EvidenceFileID != next {
		t.Fatalf("account = %+v, %v", got, err)
	}
	// The evidence's uploader is in finance's view only (BE-159).
	if _, ok := out["evidence_uploaded_by"]; ok {
		t.Fatalf("the org view names the evidence's uploader:\n%s", rec.Body.String())
	}
	rec, out = call(t, app, "finance_admin", http.MethodGet, "/api/v1/admin/bank-accounts", nil, nil)
	wantStatus(t, rec, http.StatusOK)
	items, _ := out["items"].([]any)
	found := false
	for _, it := range items {
		if m, _ := it.(map[string]any); m["bank_account_id"] == b.Account.ID.String() {
			found = m["evidence_uploaded_by"] == actor(t, "club_admin").UserID.String()
		}
	}
	if !found {
		t.Fatalf("admin queue lacks the evidence's uploader:\n%s", rec.Body.String())
	}

	party := e.buyer(t, "1234567890").Party
	cma := actor(t, "company_admin")
	company := org("company_a")
	old := e.files.add(cma.UserID, &company)
	d, err := e.svc.UploadDocument(ctxOf(t, cma), cma, company, party.ID, parties.DocumentInput{Kind: parties.DocTaxCertificate, FileID: old})
	if err != nil {
		t.Fatal(err)
	}
	e.infected(old)
	file := e.files.add(cma.UserID, &company)
	rec, out = call(t, app, "company_admin", http.MethodPost, orgPath("company_a", idPath(party.ID), "/documents", idPath(d.ID), "/replace-file"),
		map[string]any{"file_id": file.String()}, nil)
	wantStatus(t, rec, http.StatusOK)
	if out["document_id"] != d.ID.String() || out["file_state"] != "clean" {
		t.Fatalf("document answer:\n%s", rec.Body.String())
	}
}
