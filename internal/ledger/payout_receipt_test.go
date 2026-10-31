package ledger_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/files/filestest"
	"koro/internal/ledger"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// TestOrgPayoutReceipt_PaidOnlyAudited (BE-127): downloadOrgPayoutReceipt
// gives the seller org's managers a 5-minute URL of a paid payout's receipt
// and audits each download; an unpaid payout has none (409); another
// org's payout is 404; a member without the role is refused.
func TestOrgPayoutReceipt_PaidOnlyAudited(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	p, _, _ := e.approved(t)
	club := actor(t, "club_admin")
	org := authztest.ID("club_a")
	_, err := e.svc.OrgPayoutReceipt(ctxOf(t, club), club, org, p.ID)
	problemtest.WantErr(t, err, ledger.ErrInvalidState)
	v := e.markPaid(t, "finance_admin_2", p.ID)
	before := len(e.audit.Of(authz.LedgerPayoutRead))
	d, err := e.svc.OrgPayoutReceipt(ctxOf(t, club), club, org, p.ID)
	if err != nil || d.URL == "" || d.ExpiresAt.IsZero() {
		t.Fatalf("receipt = %+v, %v", d, err)
	}
	recs := e.audit.Of(authz.LedgerPayoutRead)
	if len(recs) != before+1 || recs[len(recs)-1].Metadata["receipt_file_id"] != v.ReceiptFileID.String() {
		t.Fatalf("download audit = %+v", recs)
	}
	_, err = e.svc.OrgPayoutReceipt(ctxOf(t, club), club, authztest.ID("club_b"), p.ID)
	if err == nil {
		t.Fatal("another org's surface served the receipt")
	}
	other := actor(t, "other_club_admin")
	_, err = e.svc.OrgPayoutReceipt(ctxOf(t, other), other, authztest.ID("club_b"), p.ID)
	problemtest.WantErr(t, err, ledger.ErrNotFound)
	member := actor(t, "club_member")
	_, err = e.svc.OrgPayoutReceipt(ctxOf(t, member), member, org, p.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

// TestLedger_FileRejected_KeepsReceipt (T-176, BE-151): a paid payout's
// receipt the scan rejected afterwards (infected, or unprocessable) stays
// referenced: the payout is a financial record and ledger does not consume
// file.rejected. The payout still reads (org and admin surfaces) with its
// receipt_file_id and without a link; the receipt download answers 409.
func TestLedger_FileRejected_KeepsReceipt(t *testing.T) {
	t.Parallel()
	for _, reason := range []files.RejectReason{files.RejectInfected, files.RejectUnprocessable} {
		e := newPayoutEnv(t)
		p, _, _ := e.approved(t)
		paid := e.markPaid(t, "finance_admin_2", p.ID)
		e.files.reject(*paid.ReceiptFileID, reason)
		club, fin := actor(t, "club_admin"), actor(t, "finance_admin")
		org := authztest.ID("club_a")
		v, err := e.svc.GetOrgPayout(ctxOf(t, club), club, org, p.ID)
		if err != nil || v.ReceiptFileID == nil || *v.ReceiptFileID != *paid.ReceiptFileID || v.ReceiptURL != "" {
			t.Fatalf("%s: org payout = %+v, %v", reason, v, err)
		}
		if v, err := e.svc.AdminGetPayout(ctxOf(t, fin), fin, p.ID); err != nil || v.ReceiptFileID == nil || v.ReceiptURL != "" {
			t.Fatalf("%s: admin payout = %+v, %v", reason, v, err)
		}
		_, err = e.svc.OrgPayoutReceipt(ctxOf(t, club), club, org, p.ID)
		want := files.ErrNotClean
		if reason == files.RejectUnprocessable {
			want = files.ErrRejected
		}
		problemtest.WantErr(t, err, want)
	}
}

// TestLedger_RejectedReceipt_StateNoticeReplace (BE-154, T-393, Q-37): a
// paid payout's receipt shows its file state (clean while it is served);
// replacing a receipt that was not rejected is 409 files.not_rejected. Once
// the scan rejects it, the payout reads receipt_file_state = rejected on
// the org and admin surfaces (one file read for a list); file.rejected
// tells every finance admin (the admin link) and the seller org's admins
// (the org link), one notice per recipient however often it is delivered.
// Only the people who record the transfer replace it (403 for the club
// admin and for other staff; 422 for a file of another purpose); the new
// receipt is attached to the payout, the old row stays on record (audit:
// both IDs and the reason), the state is clean again and the receipt is
// downloaded. A late file.rejected of the replaced file sends nothing; an
// unpaid payout has no receipt to replace (409).
func TestLedger_RejectedReceipt_StateNoticeReplace(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	p, _, _ := e.approved(t)
	paid := e.markPaid(t, "finance_admin_2", p.ID)
	old := *paid.ReceiptFileID
	club, fin := actor(t, "club_admin"), actor(t, "finance_admin")
	org := authztest.ID("club_a")
	if paid.ReceiptState != files.StateClean {
		t.Fatalf("receipt state after mark-paid = %q, want clean", paid.ReceiptState)
	}
	fresh := e.files.put(files.PurposePayoutReceipt)
	_, err := e.svc.AdminReplacePayoutReceipt(ctxOf(t, fin), fin, p.ID, fresh)
	problemtest.WantErr(t, err, files.ErrNotRejected)

	e.files.reject(old, files.RejectInfected)
	v, err := e.svc.GetOrgPayout(ctxOf(t, club), club, org, p.ID)
	if err != nil || v.ReceiptState != files.StateRejected {
		t.Fatalf("org payout = %+v, %v; want receipt state rejected", v, err)
	}
	page, err := e.svc.AdminListPayouts(ctxOf(t, fin), fin, ledger.PayoutQuery{})
	if err != nil || files.StateOf(page.ReceiptStates, &old) != files.StateRejected {
		t.Fatalf("admin list states = %+v, %v", page.ReceiptStates, err)
	}
	for range 2 { // at least once
		if err := filestest.Reject(t, e.mod.Subscribe, ledger.NotifyRejectedReceiptTaskName, old, files.PurposePayoutReceipt,
			files.RejectInfected, "ledger", p.ID); err != nil {
			t.Fatal(err)
		}
	}
	got := map[uuid.UUID]map[string]bool{} // recipient → dedupe keys
	links := map[uuid.UUID]string{}
	for _, m := range e.sender.all() {
		if m.Template != "ledger.payout_receipt_rejected" {
			continue
		}
		if got[*m.UserID] == nil {
			got[*m.UserID] = map[string]bool{}
			links[*m.UserID] = m.LinkPath.String()
		}
		got[*m.UserID][m.DedupeKey] = true
	}
	want := map[string]string{
		"finance_admin": "/admin/payouts/" + p.ID.String(), "finance_admin_2": "/admin/payouts/" + p.ID.String(),
		"club_admin": "/orgs/" + org.String() + "/payouts/" + p.ID.String(),
	}
	if len(got) != len(want) {
		t.Fatalf("receipt notices to %v, want %v", links, want)
	}
	for name, link := range want {
		id := authztest.ID(name)
		if len(got[id]) != 1 || links[id] != link {
			t.Fatalf("%s: dedupe keys %v, link %q; want one notice linking %s", name, got[id], links[id], link)
		}
	}

	_, err = e.svc.AdminReplacePayoutReceipt(ctxOf(t, club), club, p.ID, fresh)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	mod := actor(t, "moderator")
	_, err = e.svc.AdminReplacePayoutReceipt(ctxOf(t, mod), mod, p.ID, fresh)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminReplacePayoutReceipt(ctxOf(t, fin), fin, p.ID, e.files.put(files.PurposeTransferReceipt))
	problemtest.WantErr(t, err, problem.ErrValidation)

	v, err = e.svc.AdminReplacePayoutReceipt(ctxOf(t, fin), fin, p.ID, fresh)
	if err != nil || v.ReceiptFileID == nil || *v.ReceiptFileID != fresh || v.ReceiptState != files.StateClean ||
		v.Status != ledger.StatusPaid || v.BankReference == nil || *v.BankReference != "EFT-1" {
		t.Fatalf("replaced = %+v, %v", v, err)
	}
	owner := files.Owner{Module: "ledger", EntityID: p.ID, Purpose: files.PurposePayoutReceipt}
	if e.files.attached[fresh] != owner || e.files.attached[old] != owner {
		t.Fatalf("attachments = %+v; the new and the old receipt belong to the payout", e.files.attached)
	}
	recs := e.audit.Of(authz.LedgerPayoutMarkPaid)
	last := recs[len(recs)-1].Metadata
	if last["operation"] != "replace_rejected_file" || last["replaced_file_id"] != old.String() ||
		last["file_id"] != fresh.String() || last["replaced_reason"] != "infected" {
		t.Fatalf("replace audit = %+v", last)
	}
	if d, err := e.svc.OrgPayoutReceipt(ctxOf(t, club), club, org, p.ID); err != nil || d.URL == "" {
		t.Fatalf("receipt after the replacement = %+v, %v", d, err)
	}
	sent := len(e.sender.all())
	if err := filestest.Reject(t, e.mod.Subscribe, ledger.NotifyRejectedReceiptTaskName, old, files.PurposePayoutReceipt,
		files.RejectInfected, "ledger", p.ID); err != nil || len(e.sender.all()) != sent {
		t.Fatalf("late file.rejected of the replaced receipt: %v, %d new notices", err, len(e.sender.all())-sent)
	}
	_, err = e.svc.AdminReplacePayoutReceipt(ctxOf(t, fin), fin, p.ID, e.files.put(files.PurposePayoutReceipt))
	problemtest.WantErr(t, err, files.ErrNotRejected)

	unpaid, _, _ := e.approved(t)
	_, err = e.svc.AdminReplacePayoutReceipt(ctxOf(t, fin), fin, unpaid.ID, e.files.put(files.PurposePayoutReceipt))
	problemtest.WantErr(t, err, ledger.ErrInvalidState)
}
