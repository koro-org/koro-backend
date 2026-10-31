package ledger_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/ledger"
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
