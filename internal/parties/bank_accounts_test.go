package parties_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/parties"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem/problemtest"
)

// Test IBANs (shared vectors of platform/iban; never real accounts).
const (
	ibanA = "TR330006100519786457841326"
	ibanB = "TR950001200945200058000003"
	ibanC = "TR809999900000000000000001"
	// ibanBad has a wrong check digit (mod-97 fails).
	ibanBad = "TR330006100519786457841327"
)

// payee creates a verified foundation party, its payee for club_a (as
// finance_admin) and activates it (as platform_admin).
func (e *env) payee(t *testing.T, vkn string) parties.PayeeView {
	t.Helper()
	party := e.verify(t, e.foundationOnly(t, vkn).Party.ID)
	fin, pad := actor(t, "finance_admin"), actor(t, "platform_admin")
	v, err := e.svc.AdminCreatePayee(ctxOf(t, fin), fin, parties.PayeeInput{LegalPartyID: party.Party.ID, OrgID: org("club_a"),
		IsDefault: true})
	if err != nil {
		t.Fatalf("AdminCreatePayee: %v", err)
	}
	if v, err = e.svc.AdminVerifyPayee(ctxOf(t, pad), pad, v.Payee.ID); err != nil {
		t.Fatalf("AdminVerifyPayee: %v", err)
	}
	return v
}

// foundationOnly creates a foundation payee party without a payee row.
func (e *env) foundationOnly(t *testing.T, vkn string) parties.PartyView {
	t.Helper()
	a := actor(t, "finance_admin")
	v, err := e.svc.AdminCreateParty(ctxOf(t, a), a, foundationInput(vkn))
	if err != nil {
		t.Fatalf("AdminCreateParty: %v", err)
	}
	return v
}

// accountInput is a submission with a fresh evidence file of uploader for club_a.
func (e *env) accountInput(t *testing.T, number string, uploader string) parties.BankAccountInput {
	t.Helper()
	club := org("club_a")
	file := e.files.addPurpose(files.PurposeBankAccountEvidence, actor(t, uploader).UserID, &club)
	return parties.BankAccountInput{IBAN: number, HolderName: "ÖRNEK EĞİTİM VAKFI", BankName: "Örnek Bank", EvidenceFileID: file}
}

// orgSubmit submits number for the payee as club_admin (pending_verification).
func (e *env) orgSubmit(t *testing.T, payeeID uuid.UUID, number string) parties.BankAccountView {
	t.Helper()
	cla := actor(t, "club_admin")
	v, err := e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), payeeID, e.accountInput(t, number, "club_admin"), proof())
	if err != nil {
		t.Fatalf("SubmitPayeeBankAccount: %v", err)
	}
	return v
}

func (e *env) verifyAccount(t *testing.T, who string, id uuid.UUID) (parties.BankAccountView, error) {
	t.Helper()
	a := actor(t, who)
	return e.svc.AdminVerifyBankAccount(ctxOf(t, a), a, id, parties.VerifyInput{HolderMatchesParty: true, EvidenceOK: true})
}

func (e *env) approveAccount(t *testing.T, who string, id uuid.UUID) (parties.BankAccountView, error) {
	t.Helper()
	a := actor(t, who)
	return e.svc.AdminApproveBankAccount(ctxOf(t, a), a, id, proof())
}

// approved runs number through the org flow to approved: club_admin
// submits, finance_admin verifies, platform_admin approves.
func (e *env) approved(t *testing.T, payeeID uuid.UUID, number string) parties.BankAccountView {
	t.Helper()
	b := e.orgSubmit(t, payeeID, number)
	if _, err := e.verifyAccount(t, "finance_admin", b.Account.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	v, err := e.approveAccount(t, "platform_admin", b.Account.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return v
}

// handle runs the parties subscriptions of every recorded outbox event in a
// worker context.
func (e *env) handle(t *testing.T) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	for _, ev := range e.outbox.Events() {
		for _, s := range r.For(ev.Topic) {
			if err := s.Handle(t.Context(), ev); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	for _, task := range e.queue.Tasks() {
		for _, h := range e.mod.Tasks() {
			if h.Name == task.Type {
				if err := h.Handle(t.Context(), task.Payload); err != nil {
					t.Fatalf("%s: %v", h.Name, err)
				}
			}
		}
	}
}

// recipients returns the users a template reached on a channel.
func recipients(msgs []notify.Message, tmpl string, ch notify.Channel) []uuid.UUID {
	var out []uuid.UUID
	for _, m := range msgs {
		if m.Template == tmpl && m.Channel == ch && m.UserID != nil {
			out = append(out, *m.UserID)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func personaIDs(names ...string) []uuid.UUID {
	out := make([]uuid.UUID, len(names))
	for i, n := range names {
		out[i] = authztest.ID(n)
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// TestBankAccount_OrgSubmit_VerifyApprove_DistinctPeople pins the org flow
// of 03 §6.1: club_admin submits (pending_verification, R), finance_admin
// verifies (pending_approval), platform_admin approves (approved); the row
// names three different people and its org comes from the payee.
func TestBankAccount_OrgSubmit_VerifyApprove_DistinctPeople(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.orgSubmit(t, py.Payee.ID, "tr33 0006 1005 1978 6457 8413 26")
	if b.Account.Status != parties.AccountPendingVerification || b.Account.IBAN != ibanA || b.Account.SubmittedByAdmin ||
		b.Account.OrgID != org("club_a") || b.Account.BankCode != "00061" || b.FullIBAN {
		t.Fatalf("submitted = %+v (full %v)", b.Account, b.FullIBAN)
	}
	if _, err := e.payees().ApprovedAccount(t.Context(), py.Payee.ID); !errors.Is(err, parties.ErrNoApprovedAccount) {
		t.Fatalf("ApprovedAccount before the eyes: %v, want parties.no_approved_account", err)
	}
	v, err := e.verifyAccount(t, "finance_admin", b.Account.ID)
	if err != nil || v.Account.Status != parties.AccountPendingApproval || *v.Account.VerifiedBy != authztest.ID("finance_admin") {
		t.Fatalf("verify = %+v, %v", v.Account, err)
	}
	if _, err := e.payees().ApprovedAccount(t.Context(), py.Payee.ID); !errors.Is(err, parties.ErrNoApprovedAccount) {
		t.Fatalf("ApprovedAccount after one eye: %v", err)
	}
	v, err = e.approveAccount(t, "platform_admin", b.Account.ID)
	if err != nil || v.Account.Status != parties.AccountApproved {
		t.Fatalf("approve = %+v, %v", v.Account, err)
	}
	a := v.Account
	people := []uuid.UUID{a.SubmittedBy, *a.VerifiedBy, *a.ApprovedBy}
	if people[0] != authztest.ID("club_admin") || people[1] == people[0] || people[2] == people[0] || people[2] == people[1] {
		t.Fatalf("the eyes are not three different people: %v", people)
	}
	got, err := e.payees().ApprovedAccount(t.Context(), py.Payee.ID)
	if err != nil || got.ID != a.ID || got.IBAN != ibanA {
		t.Fatalf("ApprovedAccount = %+v, %v", got, err)
	}
	if n := len(e.audit.Of(authz.PartiesBankAccountSubmit)) + len(e.audit.Of(authz.PartiesBankAccountVerify)) +
		len(e.audit.Of(authz.PartiesBankAccountApprove)); n != 3 {
		t.Fatalf("audit records = %d, want 3", n)
	}
	if ev := e.audit.Of(authz.PartiesBankAccountApprove)[0]; ev.Metadata[audit.MetaReauth] != "password" {
		t.Fatalf("approve audit metadata = %v", ev.Metadata)
	}
	if got := e.outbox.Topics(); !slices.Equal(got, []string{domainevent.TopicPayeeBankAccountSubmitted,
		domainevent.TopicPayeeBankAccountSubmitted, domainevent.TopicPayeeBankAccountApproved}) {
		t.Fatalf("events = %v", got)
	}
}

func (e *env) payees() parties.Payees { return e.mod.Payees }

// TestBankAccount_VerifierApprovesOwn_403FourEyes: the first eye may not be
// the second (03 §6.1 B ≠ A), whichever admin role it holds.
func TestBankAccount_VerifierApprovesOwn_403FourEyes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.orgSubmit(t, py.Payee.ID, ibanA)
	if _, err := e.verifyAccount(t, "finance_admin", b.Account.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.approveAccount(t, "finance_admin", b.Account.ID)
	problemtest.WantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	if len(e.reauth.calls) != 1 { // the submission's: the refused approval never asked for the proof
		t.Fatalf("reauth calls = %d", len(e.reauth.calls))
	}
	cur, _ := e.store.GetBankAccount(t.Context(), b.Account.ID, false)
	if cur.Status != parties.AccountPendingApproval || cur.ApprovedBy != nil {
		t.Fatalf("after the refusal = %+v", cur)
	}
	if _, err := e.approveAccount(t, "platform_admin", b.Account.ID); err != nil {
		t.Fatalf("another admin approves: %v", err)
	}
}

// TestBankAccount_SubmitterVerifies_403: the submitter is never the first
// eye, even when they later became a finance admin (Y6): authz refuses it
// and the store's CHECK (payee_bank_accounts_verifier_submitter_chk) too.
func TestBankAccount_SubmitterVerifies_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.orgSubmit(t, py.Payee.ID, ibanA)
	// The club admin who submitted is granted finance_admin afterwards.
	turned := actor(t, "club_admin")
	turned.PlatformRoles, turned.MFAVerified = []authz.PlatformRole{authz.PlatformFinanceAdmin}, true
	_, err := e.svc.AdminVerifyBankAccount(ctxOf(t, turned), turned, b.Account.ID,
		parties.VerifyInput{HolderMatchesParty: true, EvidenceOK: true})
	problemtest.WantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	// The second layer: the store refuses the same row.
	next := b.Account
	now := e.clk.Now().UTC()
	next.Status, next.VerifiedBy, next.VerifiedAt = parties.AccountPendingApproval, &turned.UserID, &now
	_, err = e.store.TransitionBankAccount(t.Context(), next, parties.AccountPendingVerification)
	problemtest.WantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	// Nobody without a platform role is an eye: 403 before the account is read.
	cla := actor(t, "club_admin")
	_, err = e.svc.AdminVerifyBankAccount(ctxOf(t, cla), cla, uuid.NewV7(), parties.VerifyInput{HolderMatchesParty: true, EvidenceOK: true})
	problemtest.WantProblem(t, err, http.StatusForbidden, "forbidden")
}

// TestBankAccount_AdminSubmit_SameAdminApprove_403: an admin submission
// starts pending_approval (the submitter is the first eye) and needs a
// different admin's approval.
func TestBankAccount_AdminSubmit_SameAdminApprove_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	fin := actor(t, "finance_admin")
	b, err := e.svc.AdminSubmitBankAccount(ctxOf(t, fin), fin, py.Payee.ID, e.accountInput(t, ibanA, "finance_admin"), proof())
	if err != nil || b.Account.Status != parties.AccountPendingApproval || !b.Account.SubmittedByAdmin || !b.FullIBAN {
		t.Fatalf("admin submit = %+v, %v", b.Account, err)
	}
	_, err = e.verifyAccount(t, "platform_admin", b.Account.ID) // no first eye for an admin submission
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	_, err = e.approveAccount(t, "finance_admin", b.Account.ID)
	problemtest.WantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	// The store refuses the same approver (payee_bank_accounts_approver_submitter_chk).
	next := b.Account
	now := e.clk.Now().UTC()
	next.Status, next.ApprovedBy, next.ApprovedAt = parties.AccountApproved, &fin.UserID, &now
	_, err = e.store.TransitionBankAccount(t.Context(), next, parties.AccountPendingApproval)
	problemtest.WantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	if v, err := e.approveAccount(t, "platform_admin", b.Account.ID); err != nil || v.Account.Status != parties.AccountApproved {
		t.Fatalf("another admin approves = %+v, %v", v.Account, err)
	}
}

// txMark marks the context of each outermost WithTx so a store wrapper can
// tell which writes shared a transaction.
type txMark struct{}

type markingTx struct {
	db.NopTx
	n *int
}

func (m markingTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if db.InTx(ctx) {
		return m.NopTx.WithTx(ctx, fn)
	}
	*m.n++
	id := *m.n
	return m.NopTx.WithTx(ctx, func(ctx context.Context) error { return fn(context.WithValue(ctx, txMark{}, id)) })
}

// recordingStore records the transaction of each bank account transition.
type recordingStore struct {
	*parties.MemStore
	txOf map[uuid.UUID][]any
}

func (r recordingStore) TransitionBankAccount(ctx context.Context, b parties.BankAccount, from parties.BankAccountStatus) (parties.BankAccount, error) {
	r.txOf[b.ID] = append(r.txOf[b.ID], ctx.Value(txMark{}))
	return r.MemStore.TransitionBankAccount(ctx, b, from)
}

// TestBankAccount_ApproveSupersedesOldInSameTx: approving a change
// supersedes the approved account in the approval's transaction (the
// supersede first: one approved account per payee at any moment); the old
// row keeps its IBAN, points at its successor and can no longer be paid or
// revoked. (The rollback of both together runs against PostgreSQL in
// TestBankAccount_Gorm_ApproveRollsBackTogether.)
func TestBankAccount_ApproveSupersedesOldInSameTx(t *testing.T) {
	t.Parallel()
	n := 0
	rec := recordingStore{txOf: map[uuid.UUID][]any{}}
	e := newEnv(t, func(d *parties.Deps) {
		rec.MemStore = d.Store.(*parties.MemStore)
		tx := markingTx{n: &n}
		d.Store, d.Tx = rec, tx
		d.Idempotency = idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: d.Clock, Log: discard()})
	})
	py := e.payee(t, vknC)
	old := e.approved(t, py.Payee.ID, ibanA)
	b := e.orgSubmit(t, py.Payee.ID, ibanB)
	if _, err := e.verifyAccount(t, "finance_admin", b.Account.ID); err != nil {
		t.Fatal(err)
	}
	v, err := e.approveAccount(t, "platform_admin", b.Account.ID)
	if err != nil || v.Account.Status != parties.AccountApproved {
		t.Fatalf("approve = %+v, %v", v.Account, err)
	}
	prev, _ := e.store.GetBankAccount(t.Context(), old.Account.ID, false)
	if prev.Status != parties.AccountSuperseded || prev.SupersededByID == nil || *prev.SupersededByID != b.Account.ID ||
		prev.SupersededAt == nil || prev.IBAN != ibanA || prev.ApprovedBy == nil {
		t.Fatalf("old account = %+v", prev)
	}
	supersedeTx := rec.txOf[old.Account.ID][len(rec.txOf[old.Account.ID])-1]
	approveTx := rec.txOf[b.Account.ID][len(rec.txOf[b.Account.ID])-1]
	if supersedeTx == nil || supersedeTx != approveTx {
		t.Fatalf("supersede in tx %v, approval in tx %v: not one transaction", supersedeTx, approveTx)
	}
	got, err := e.payees().ApprovedAccount(t.Context(), py.Payee.ID)
	if err != nil || got.ID != b.Account.ID || got.IBAN != ibanB {
		t.Fatalf("ApprovedAccount = %+v, %v", got, err)
	}
	all, _ := e.store.PayeeBankAccounts(t.Context(), py.Payee.ID)
	approvedN := 0
	for _, a := range all {
		if a.Status == parties.AccountApproved {
			approvedN++
		}
	}
	if approvedN != 1 {
		t.Fatalf("%d approved accounts", approvedN)
	}
	meta := e.audit.Of(authz.PartiesBankAccountApprove)[1].Metadata
	if meta["superseded_bank_account_id"] != old.Account.ID.String() {
		t.Fatalf("approve audit = %v", meta)
	}
	pad := actor(t, "platform_admin")
	_, err = e.svc.AdminRevokeBankAccount(ctxOf(t, pad), pad, old.Account.ID, "Eski hesap", proof())
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
}

// TestBankAccount_HolderMismatchWithoutNote_422 (T-19): a holder that is
// not the legal party's title needs the first eye's note — when the eye
// says so, and when the folded names differ although the eye ticked the
// box; with the note the account goes on and the note is audited.
func TestBankAccount_HolderMismatchWithoutNote_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.orgSubmit(t, py.Payee.ID, ibanA) // holder "ÖRNEK EĞİTİM VAKFI" = title "Örnek Eğitim Vakfı"
	if !b.HolderMatchesTitle {
		t.Fatal("the folded holder should match the title")
	}
	fin := actor(t, "finance_admin")
	_, err := e.svc.AdminVerifyBankAccount(ctxOf(t, fin), fin, b.Account.ID, parties.VerifyInput{EvidenceOK: true})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	_, err = e.svc.AdminVerifyBankAccount(ctxOf(t, fin), fin, b.Account.ID, parties.VerifyInput{HolderMatchesParty: true})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed") // the evidence must hold: reject instead

	// A holder that differs from the title: ticking "matches" is not enough.
	cla := actor(t, "club_admin")
	if _, err := e.svc.AdminRejectBankAccount(ctxOf(t, fin), fin, b.Account.ID, "Yeniden gönderilecek"); err != nil {
		t.Fatal(err)
	}
	in := e.accountInput(t, ibanB, "club_admin")
	in.HolderName = "Ahmet Yılmaz"
	other, err := e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), py.Payee.ID, in, proof())
	if err != nil || other.HolderMatchesTitle {
		t.Fatalf("submit = %+v, %v", other, err)
	}
	_, err = e.svc.AdminVerifyBankAccount(ctxOf(t, fin), fin, other.Account.ID, parties.VerifyInput{HolderMatchesParty: true, EvidenceOK: true})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	if cur, _ := e.store.GetBankAccount(t.Context(), other.Account.ID, false); cur.Status != parties.AccountPendingVerification {
		t.Fatalf("status after the refusal = %s", cur.Status)
	}
	note := "Vakfın kulüp alt hesabı; yetki yazısı ekte"
	v, err := e.svc.AdminVerifyBankAccount(ctxOf(t, fin), fin, other.Account.ID, parties.VerifyInput{EvidenceOK: true, Note: note})
	if err != nil || v.Account.Status != parties.AccountPendingApproval {
		t.Fatalf("verify with a note = %+v, %v", v.Account, err)
	}
	ev := e.audit.Of(authz.PartiesBankAccountVerify)[0]
	if ev.Reason != note || ev.Metadata["holder_matches_party"] != false || ev.Metadata["holder_matches_title"] != false {
		t.Fatalf("verify audit = %+v", ev)
	}
}

// TestBankAccount_InvalidIBAN_422: TR + 24 digits with the mod-97 check
// (platform/iban); the database CHECK is the second layer and answers the
// same code.
func TestBankAccount_InvalidIBAN_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	cla := actor(t, "club_admin")
	for _, bad := range []string{ibanBad, "DE89370400440532013000", "TR33000610051978645784132", "TR33 0006 1005 1978 6457 8413 2X", ""} {
		_, err := e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), py.Payee.ID, e.accountInput(t, bad, "club_admin"), proof())
		problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.iban_invalid")
	}
	if len(e.reauth.calls) != 0 {
		t.Fatal("an invalid IBAN must be refused before the re-authentication")
	}
	_, err := e.store.InsertBankAccount(t.Context(), parties.BankAccount{ID: uuid.NewV7(), PayeeID: py.Payee.ID,
		OrgID: py.Payee.OrgID, IBAN: ibanBad, HolderName: "Örnek", EvidenceFileID: uuid.NewV7(),
		Status: parties.AccountPendingVerification, SubmittedBy: cla.UserID})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.iban_invalid")
}

// TestBankAccount_MaskedForOrgRoles: club admins and university reps see
// the IBAN masked (list, payee view, submission answer), the HTTP body
// carries no full IBAN; finance and platform admins see it in full.
func TestBankAccount_MaskedForOrgRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	e.approved(t, py.Payee.ID, ibanA)
	app := newHTTP(t, e, nil)
	base := "/api/v1/orgs/" + org("club_a").String() + "/payees"
	for _, path := range []string{base, base + "/" + py.Payee.ID.String(), base + "/" + py.Payee.ID.String() + "/bank-accounts"} {
		rec, _ := call(t, app, "club_admin", http.MethodGet, path, nil, nil)
		wantStatus(t, rec, http.StatusOK)
		body := rec.Body.String()
		if strings.Contains(body, ibanA) || strings.Contains(body, `"iban"`) || !strings.Contains(body, "TR33 **** **** **** **** 8413 26") {
			t.Fatalf("club_admin %s:\n%s", path, body)
		}
	}
	rec, out := call(t, app, "finance_admin", http.MethodGet, base+"/"+py.Payee.ID.String()+"/bank-accounts", nil, nil)
	wantStatus(t, rec, http.StatusOK)
	items, _ := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["iban"] != ibanA || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("finance_admin list:\n%s", rec.Body.String())
	}
	// The payee view masks for everyone (a full read is the audited list only).
	rec, _ = call(t, app, "finance_admin", http.MethodGet, base+"/"+py.Payee.ID.String(), nil, nil)
	if strings.Contains(rec.Body.String(), ibanA) {
		t.Fatalf("payee view carries the full IBAN:\n%s", rec.Body.String())
	}
	// The submission answer of an org role is masked too.
	in := e.accountInput(t, ibanB, "club_admin")
	rec, _ = call(t, app, "club_admin", http.MethodPost, base+"/"+py.Payee.ID.String()+"/bank-accounts", map[string]any{
		"iban": in.IBAN, "holder_name": in.HolderName, "evidence_file_id": in.EvidenceFileID.String(),
		"reauth": map[string]any{"password": reauthPassword},
	}, nil)
	wantStatus(t, rec, http.StatusCreated)
	if strings.Contains(rec.Body.String(), ibanB) || strings.Contains(rec.Body.String(), `"iban"`) {
		t.Fatalf("submission answer:\n%s", rec.Body.String())
	}
}

// TestBankAccount_FullIBANReadAudited (03 §6.5): every response that shows
// finance or platform admins a full IBAN is audited (the payee's list, the
// queue, the evidence); an org role's masked read is not.
func TestBankAccount_FullIBANReadAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.approved(t, py.Payee.ID, ibanA)
	pending := e.orgSubmit(t, py.Payee.ID, ibanB)
	cla, fin, pad := actor(t, "club_admin"), actor(t, "finance_admin"), actor(t, "platform_admin")
	if _, err := e.svc.ListPayeeBankAccounts(ctxOf(t, cla), cla, org("club_a"), py.Payee.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.Of(authz.PartiesBankAccountList)); n != 0 {
		t.Fatalf("a masked read was audited (%d)", n)
	}
	vs, err := e.svc.ListPayeeBankAccounts(ctxOf(t, fin), fin, org("club_a"), py.Payee.ID)
	if err != nil || len(vs) != 2 || !vs[0].FullIBAN {
		t.Fatalf("finance list = %+v, %v", vs, err)
	}
	reads := e.audit.Of(authz.PartiesBankAccountList)
	if len(reads) != 1 || reads[0].ActorUserID == nil || *reads[0].ActorUserID != fin.UserID || reads[0].Metadata["full_iban"] != true ||
		*reads[0].OrgID != org("club_a") || !slices.Contains(reads[0].Metadata["bank_account_ids"].([]string), b.Account.ID.String()) {
		t.Fatalf("full read audit = %+v", reads)
	}
	page, err := e.svc.AdminListBankAccounts(ctxOf(t, pad), pad, parties.BankAccountQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Account.ID != pending.Account.ID {
		t.Fatalf("queue = %+v, %v", page, err)
	}
	if _, err := e.svc.AdminDownloadBankAccountEvidence(ctxOf(t, pad), pad, b.Account.ID); err != nil {
		t.Fatal(err)
	}
	reads = e.audit.Of(authz.PartiesBankAccountList)
	if len(reads) != 3 || reads[1].Metadata["operation"] != "admin_queue" || reads[2].Metadata["operation"] != "evidence_download" {
		t.Fatalf("audited reads = %+v", reads)
	}
	for _, ev := range e.audit.All() {
		if strings.Contains(dump(ev), ibanA) || strings.Contains(dump(ev), ibanB) {
			t.Fatalf("an audit record holds a full IBAN: %+v", ev)
		}
	}
	noIBAN(t, e.logs.String())
}

// noIBAN fails when s holds one of the test IBANs (also grouped).
func noIBAN(t *testing.T, s string) {
	t.Helper()
	for _, n := range []string{ibanA, ibanB, ibanC} {
		grouped := n[:4] + " " + n[4:8]
		if strings.Contains(s, n) || strings.Contains(s, grouped) {
			t.Fatalf("logs hold an IBAN:\n%s", s)
		}
	}
}

// TestBankAccount_ApproveRequiresReauth (03 §6.2): the second eye proves
// who they are in the same request — no proof 403
// identity.reauth_required, a wrong one 403 identity.reauth_failed, nothing
// changes; without a verifier the operation fails closed (503). Submission
// and revocation are R too.
func TestBankAccount_ApproveRequiresReauth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.orgSubmit(t, py.Payee.ID, ibanA)
	if _, err := e.verifyAccount(t, "finance_admin", b.Account.ID); err != nil {
		t.Fatal(err)
	}
	pad := actor(t, "platform_admin")
	_, err := e.svc.AdminApproveBankAccount(ctxOf(t, pad), pad, b.Account.ID, identity.ReauthProof{})
	problemtest.WantProblem(t, err, http.StatusForbidden, "identity.reauth_required")
	_, err = e.svc.AdminApproveBankAccount(ctxOf(t, pad), pad, b.Account.ID, identity.ReauthProof{Password: "yanlış-parola"})
	problemtest.WantProblem(t, err, http.StatusForbidden, "identity.reauth_failed")
	if cur, _ := e.store.GetBankAccount(t.Context(), b.Account.ID, false); cur.Status != parties.AccountPendingApproval {
		t.Fatalf("status after refused proofs = %s", cur.Status)
	}
	if _, err := e.svc.AdminApproveBankAccount(ctxOf(t, pad), pad, b.Account.ID, proof()); err != nil {
		t.Fatal(err)
	}
	cla := actor(t, "club_admin")
	_, err = e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), py.Payee.ID, e.accountInput(t, ibanB, "club_admin"),
		identity.ReauthProof{})
	problemtest.WantProblem(t, err, http.StatusForbidden, "identity.reauth_required")
	_, err = e.svc.AdminRevokeBankAccount(ctxOf(t, pad), pad, b.Account.ID, "Hesap kapandı", identity.ReauthProof{})
	problemtest.WantProblem(t, err, http.StatusForbidden, "identity.reauth_required")

	closed := newEnv(t, func(d *parties.Deps) { d.Reauth = nil })
	cpy := closed.payee(t, vknC)
	_, err = closed.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), cpy.Payee.ID, closed.accountInput(t, ibanA, "club_admin"), proof())
	problemtest.WantProblem(t, err, http.StatusServiceUnavailable, "unavailable")
}

// TestApproved_SecurityNotificationToAllFinance: an approval tells every
// finance admin in effect and every admin of the payee's org (security
// notices: in-app and e-mail with the masked IBAN); the submission already
// told them (an IBAN change they did not make is an account takeover) and
// the finance admins that an eye is needed; a revocation is notified the
// same way, a rejection to the submitter.
func TestApproved_SecurityNotificationToAllFinance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.approved(t, py.Payee.ID, ibanA)
	e.handle(t)
	sent := e.sender.all()
	finance := personaIDs("finance_admin", "finance_admin_2")
	orgAdmins := personaIDs("club_admin")
	both := append(slices.Clone(finance), orgAdmins...)
	slices.SortFunc(both, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, ch := range []notify.Channel{notify.ChannelInApp, notify.ChannelEmail} {
		if got := recipients(sent, "parties.bank_account_approved", ch); !slices.Equal(got, both) {
			t.Errorf("approved %s → %v, want finance admins and the club's admins %v", ch, got, both)
		}
		if got := recipients(sent, "parties.bank_account_changed", ch); !slices.Equal(got, orgAdmins) {
			t.Errorf("changed %s → %v, want the club's admins %v", ch, got, orgAdmins)
		}
	}
	pending := 0
	for _, m := range sent {
		if m.Template == "parties.bank_account_pending" && m.Channel == notify.ChannelEmail {
			pending++
		}
	}
	if pending != 4 { // both finance admins, once per stage (verification, approval)
		t.Errorf("pending notices = %d, want 4", pending)
	}
	tmpls, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range sent {
		switch m.Channel {
		case notify.ChannelEmail:
			r, err := tmpls.Render(m.Template, m.Locale, notify.RenderInput{Data: m.Data})
			if err != nil {
				t.Fatalf("%s: %v", m.Template, err)
			}
			if !strings.Contains(r.Text, b.Account.MaskedIBAN()) || strings.Contains(r.Text+r.HTML+r.Subject, ibanA) {
				t.Fatalf("%s mail must carry the masked IBAN only:\n%s", m.Template, r.Text)
			}
		case notify.ChannelInApp:
			if _, ok := tmpls.Notice(m.Template, "tr"); !ok || len(m.Data) != 2 {
				t.Fatalf("%s in-app notice: data %v", m.Template, m.Data)
			}
		}
	}
	// A repeated task sends nothing again (dedupe keys).
	before := len(e.sender.all())
	e.handle(t)
	if len(e.sender.all()) != before {
		t.Fatal("a repeated notice task sent again")
	}
	// Revocation: org admins and finance admins; payouts stop at once.
	pad := actor(t, "platform_admin")
	if _, err := e.svc.AdminRevokeBankAccount(ctxOf(t, pad), pad, b.Account.ID, "Vakıf hesabın kapandığını bildirdi", proof()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.payees().ApprovedAccount(t.Context(), py.Payee.ID); !errors.Is(err, parties.ErrNoApprovedAccount) {
		t.Fatalf("ApprovedAccount after the revocation: %v", err)
	}
	e.handle(t)
	if got := recipients(e.sender.all(), "parties.bank_account_revoked", notify.ChannelEmail); !slices.Equal(got, both) {
		t.Errorf("revoked → %v, want %v", got, both)
	}
	// Rejection: the submitter.
	r := e.orgSubmit(t, py.Payee.ID, ibanC)
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminRejectBankAccount(ctxOf(t, fin), fin, r.Account.ID, "Belge okunmuyor"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.outbox.Topics(), domainevent.TopicPayeeBankAccountRejected) {
		t.Fatal("no rejected event")
	}
	e.handle(t)
	if got := recipients(e.sender.all(), "parties.bank_account_rejected", notify.ChannelInApp); !slices.Equal(got, personaIDs("club_admin")) {
		t.Errorf("rejected → %v, want the submitter", got)
	}
	noIBAN(t, e.logs.String())
}

// TestBankAccount_SubmissionRules: one pending submission per payee (409),
// a closed payee takes none (409), the evidence must be a
// bank_account_evidence upload of the caller or the org (422), another
// org's payee is 404 (06 H-38) and a natural person's payee is 422 (06
// H-54).
func TestBankAccount_SubmissionRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	cla := actor(t, "club_admin")
	e.orgSubmit(t, py.Payee.ID, ibanA)
	_, err := e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), py.Payee.ID, e.accountInput(t, ibanB, "club_admin"), proof())
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.bank_account_pending")

	in := e.accountInput(t, ibanB, "club_admin")
	doc := e.files.add(cla.UserID, nil) // a party_document, not evidence
	in.EvidenceFileID = doc
	other := e.payee(t, vknD)
	_, err = e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), other.Payee.ID, in, proof())
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	stranger := e.files.addPurpose(files.PurposeBankAccountEvidence, actor(t, "other_club_admin").UserID, nil)
	in.EvidenceFileID = stranger
	_, err = e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), other.Payee.ID, in, proof())
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")

	// Another org's path: 404 for its admin and for staff alike.
	uniRep := actor(t, "university_rep")
	_, err = e.svc.SubmitPayeeBankAccount(ctxOf(t, uniRep), uniRep, org("uni_a"), py.Payee.ID, e.accountInput(t, ibanB, "university_rep"), proof())
	problemtest.WantErr(t, err, parties.ErrNotFound)
	fin := actor(t, "finance_admin")
	_, err = e.svc.ListPayeeBankAccounts(ctxOf(t, fin), fin, org("uni_a"), py.Payee.ID)
	problemtest.WantErr(t, err, parties.ErrNotFound)

	// A closed payee takes no account.
	pad := actor(t, "platform_admin")
	cur, _ := e.svc.GetOrgPayee(ctxOf(t, pad), pad, org("club_a"), other.Payee.ID)
	closed := parties.PayeeClosed
	if _, err := e.svc.AdminUpdatePayee(ctxOf(t, pad), pad, other.Payee.ID, cur.ETag, parties.PayeePatch{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), other.Payee.ID, e.accountInput(t, ibanB, "club_admin"), proof())
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")

	// 06 H-54: a natural person's payee (written past the API) takes no account.
	sole := parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindPayee, LegalType: parties.LegalSoleProp, Title: "Ahmet Yılmaz",
		VATStatus: parties.VATUnknown, Status: parties.StatusPendingVerification}
	if _, err := e.store.InsertParty(t.Context(), sole); err != nil {
		t.Fatal(err)
	}
	solePayee := uuid.NewV7()
	e.store.LinkPayee(solePayee, org("club_a"), sole.ID, "active")
	_, err = e.svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, org("club_a"), solePayee, e.accountInput(t, ibanB, "club_admin"), proof())
	problemtest.WantErr(t, err, parties.ErrNaturalPersonPayee)
}

// TestBankAccount_HTTPAdminQueue pins the admin DTO: the full IBAN, the
// eyes, the party title and the advisory holder match; the queue is
// oldest first with a cursor.
func TestBankAccount_HTTPAdminQueue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.payee(t, vknC)
	second := e.payee(t, vknD)
	a := e.orgSubmit(t, first.Payee.ID, ibanA)
	b := e.orgSubmit(t, second.Payee.ID, ibanB)
	app := newHTTP(t, e, nil)
	rec, out := call(t, app, "finance_admin", http.MethodGet, "/api/v1/admin/bank-accounts?limit=1", nil, nil)
	wantStatus(t, rec, http.StatusOK)
	items := out["items"].([]any)
	item := items[0].(map[string]any)
	if len(items) != 1 || item["bank_account_id"] != a.Account.ID.String() || item["iban"] != ibanA ||
		item["submitted_by"] != authztest.ID("club_admin").String() || item["party_title"] != "Örnek Eğitim Vakfı" ||
		item["holder_matches_title"] != true || out["next_cursor"] == "" {
		t.Fatalf("queue page 1:\n%s", rec.Body.String())
	}
	rec, out = call(t, app, "finance_admin", http.MethodGet, "/api/v1/admin/bank-accounts?limit=1&cursor="+out["next_cursor"].(string), nil, nil)
	wantStatus(t, rec, http.StatusOK)
	if items := out["items"].([]any); len(items) != 1 || items[0].(map[string]any)["bank_account_id"] != b.Account.ID.String() {
		t.Fatalf("queue page 2:\n%s", rec.Body.String())
	}
	rec, _ = call(t, app, "club_admin", http.MethodGet, "/api/v1/admin/bank-accounts", nil, nil)
	wantStatus(t, rec, http.StatusForbidden)
	// The verify body's checklist and the approval's proof travel as JSON.
	rec, _ = call(t, app, "platform_admin", http.MethodPost, "/api/v1/admin/bank-accounts/"+a.Account.ID.String()+"/verify",
		map[string]any{"checklist": map[string]any{"holder_matches_party": true, "evidence_ok": true}}, nil)
	wantStatus(t, rec, http.StatusOK)
	rec, out = call(t, app, "finance_admin", http.MethodPost, "/api/v1/admin/bank-accounts/"+a.Account.ID.String()+"/approve",
		map[string]any{"reauth": map[string]any{"password": reauthPassword}}, nil)
	wantStatus(t, rec, http.StatusOK)
	if out["status"] != "approved" || out["approved_by"] != authztest.ID("finance_admin").String() {
		t.Fatalf("approve:\n%s", rec.Body.String())
	}
	var check struct {
		Items []json.RawMessage `json:"items"`
	}
	rec, _ = call(t, app, "club_admin", http.MethodGet, "/api/v1/orgs/"+org("club_a").String()+"/payees", nil, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &check); err != nil || len(check.Items) != 2 {
		t.Fatalf("payees:\n%s", rec.Body.String())
	}
}

// TestBankAccount_EvidenceMustBeViewable: neither eye decides on evidence
// nobody could look at (an unscanned or infected file is 409
// files.not_clean); an admin submission's approver checks it too.
func TestBankAccount_EvidenceMustBeViewable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payee(t, vknC)
	b := e.orgSubmit(t, py.Payee.ID, ibanA)
	e.files.set(b.Account.EvidenceFileID, func(m *files.FileMeta) { m.ScanStatus = files.ScanPending })
	_, err := e.verifyAccount(t, "finance_admin", b.Account.ID)
	problemtest.WantErr(t, err, files.ErrNotClean)
	e.files.set(b.Account.EvidenceFileID, func(m *files.FileMeta) { m.ScanStatus = files.ScanClean })
	if _, err := e.verifyAccount(t, "finance_admin", b.Account.ID); err != nil {
		t.Fatal(err)
	}
	e.files.set(b.Account.EvidenceFileID, func(m *files.FileMeta) { m.ScanStatus = files.ScanInfected })
	_, err = e.approveAccount(t, "platform_admin", b.Account.ID)
	problemtest.WantErr(t, err, files.ErrNotClean)
	if cur, _ := e.store.GetBankAccount(t.Context(), b.Account.ID, false); cur.Status != parties.AccountPendingApproval {
		t.Fatalf("status = %s", cur.Status)
	}
	pad := actor(t, "platform_admin")
	_, err = e.svc.AdminDownloadBankAccountEvidence(ctxOf(t, pad), pad, b.Account.ID)
	problemtest.WantErr(t, err, files.ErrNotClean)
}
