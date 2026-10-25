//go:build integration

package ledger_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"koro/internal/authz"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// TestMarkPaid_PaidBeforeApproval_NoticeFromCommittedRow (00 T-298, BE-092)
// over PostgreSQL: a transfer dated before the approval commits with the
// ledger, the paid payout and the payout.paid outbox row in one transaction
// (a failure at the last step leaves no row, so no notice); the committed
// row read back from the outbox drives the security notice, whose predicate
// holds on the stored timestamps (database precision). An approver unknown
// to identity is named by ID.
func TestMarkPaid_PaidBeforeApproval_NoticeFromCommittedRow(t *testing.T) {
	t.Parallel()
	e := newPgPayoutEnv(t)
	payee := e.w.payee()
	e.paidLine(t, payee)
	p := e.approvedPgPayout(t, payee)
	c := e.finance(t)
	receipt := e.receipt(t)
	early := p.ApprovedAt.Add(-time.Hour)
	in := ledger.MarkPaidInput{BankReference: "EFT-20261120-0001", ReceiptFileID: &receipt, PaidAt: &early}

	e.audit.FailOn(authz.LedgerPayoutMarkPaid, errAuditDown)
	if _, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, c), c, p.ID, in); !errors.Is(err, errAuditDown) {
		t.Fatalf("mark-paid with the audit down: %v", err)
	}
	if n := e.w.count(`SELECT count(*) FROM outbox WHERE topic = 'payout.paid' AND aggregate_id = $1`, p.ID); n != 0 {
		t.Fatalf("payout.paid rows after the rollback: %d", n)
	}
	e.audit.FailOn("", nil)
	v, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, c), c, p.ID, in)
	if err != nil {
		t.Fatalf("mark-paid: %v", err)
	}
	if !v.PaidBeforeApproval() {
		t.Fatalf("stored payout approved %v, paid %v: not flagged", v.ApprovedAt, v.PaidAt)
	}
	if rows := e.audit.Of(authz.LedgerPayoutMarkPaid); len(rows) != 1 || rows[0].Metadata["paid_before_approval"] != true {
		t.Fatalf("mark-paid audit %+v", rows)
	}

	var row outbox.Row
	if err := e.d.ORM.WithContext(t.Context()).Where("topic = ? AND aggregate_id = ?", domainevent.TopicPayoutPaid, p.ID).
		First(&row).Error; err != nil {
		t.Fatalf("payout.paid outbox row: %v", err)
	}
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	i := slices.IndexFunc(reg.For(domainevent.TopicPayoutPaid), func(s outbox.Subscription) bool {
		return s.Name == ledger.NotifyPayoutPaidBeforeApprovalTaskName
	})
	if i < 0 {
		t.Fatal("no security notice subscription")
	}
	if err := reg.For(domainevent.TopicPayoutPaid)[i].Handle(t.Context(), row.Envelope()); err != nil {
		t.Fatalf("security notice: %v", err)
	}
	var inApp, mail int
	for _, m := range e.sender.all() {
		if m.Template != "ledger.payout_paid_before_approval" {
			continue
		}
		switch m.Channel {
		case notify.ChannelInApp:
			inApp++
		case notify.ChannelEmail:
			mail++
			if m.Data["approved_by"] != p.ApprovedBy.String() || m.Data["iban_masked"] != v.MaskedIBAN() ||
				m.Data["bank_reference"] != "EFT-20261120-0001" {
				t.Errorf("e-mail %+v", m.Data)
			}
		}
		if m.UserID != nil && *m.UserID == c.UserID {
			t.Error("the admin who marked it paid was notified")
		}
	}
	// fakeStaff: two finance and two platform admins (the marker is none of
	// them); fakeUsers: three have an address.
	if inApp != 4 || mail != 3 {
		t.Fatalf("security notices: %d in-app, %d e-mail; want 4, 3", inApp, mail)
	}
}
