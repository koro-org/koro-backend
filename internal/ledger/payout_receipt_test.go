package ledger_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
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
