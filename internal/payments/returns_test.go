package payments_test

import (
	"errors"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/payments"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
)

// TestTransferReturn_SameFinanceApprove_403 (06 H-18, H-56): the return of
// an unmatched transfer is requested by one finance admin and approved by
// another (the requester gets 403 authz.four_eyes_violation, also through
// the store's CHECK); the EFT receipt closes it. A rejected request makes
// the transfer unmatched again with the request cleared.
func TestTransferReturn_SameFinanceApprove_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	tr := e.transfer(t, acct, 77_000, "tanımsız", true)
	fin, fin2 := actor(t, "finance_admin"), actor(t, "finance_admin_2")

	got, err := e.svc.RequestReturn(ctxOf(t, fin), fin, tr.ID, payments.ReturnRequest{Reason: "Sipariş yok"}, good)
	if err != nil || got.Status != payments.TransferReturnRequested || got.ReturnTargetIBAN == nil || *got.ReturnTargetIBAN != ibanSender ||
		got.ReturnRequestedBy == nil || *got.ReturnRequestedBy != fin.UserID {
		t.Fatalf("requested = %+v, %v", got, err)
	}
	for _, d := range []payments.Decision{payments.DecisionApproved, payments.DecisionRejected} {
		if _, err := e.svc.DecideReturn(ctxOf(t, fin), fin, tr.ID, d, good); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("the requester decides (%s) = %v", d, err)
		}
	}
	self := got
	by, at := fin.UserID, e.clk.Now()
	self.Status, self.ReturnApprovedBy, self.ReturnApprovedAt = payments.TransferReturnApproved, &by, &at
	if _, err := e.store.UpdateTransfer(t.Context(), self, payments.TransferReturnRequested); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("store four eyes = %v", err)
	}
	pad := actor(t, "platform_admin") // k28: finance admins only
	if _, err := e.svc.DecideReturn(ctxOf(t, pad), pad, tr.ID, payments.DecisionApproved, good); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform admin decides = %v", err)
	}
	if _, err := e.svc.DecideReturn(ctxOf(t, fin2), fin2, tr.ID, payments.DecisionApproved, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("decide without proof = %v", err)
	}
	receipt := e.files.add(files.PurposeRefundReceipt)
	if _, err := e.svc.MarkReturned(ctxOf(t, fin), fin, tr.ID, payments.ReturnReceipt{BankReference: "EFT-1", ReceiptFileID: receipt}, good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("receipt before the approval = %v", err)
	}
	got, err = e.svc.DecideReturn(ctxOf(t, fin2), fin2, tr.ID, payments.DecisionApproved, good)
	if err != nil || got.Status != payments.TransferReturnApproved || got.ReturnApprovedBy == nil || *got.ReturnApprovedBy != fin2.UserID {
		t.Fatalf("approved = %+v, %v", got, err)
	}
	// The requester may record the EFT (the destination was approved by another eye).
	got, err = e.svc.MarkReturned(ctxOf(t, fin), fin, tr.ID, payments.ReturnReceipt{BankReference: " EFT-1 ", ReceiptFileID: receipt}, good)
	if err != nil || got.Status != payments.TransferReturned || got.ReturnBankReference == nil || *got.ReturnBankReference != "EFT-1" ||
		got.ReturnReceiptFileID == nil || got.ReturnedAt == nil {
		t.Fatalf("returned = %+v, %v", got, err)
	}
	if mod, owner := e.files.owner(receipt); mod != "payments" || owner != tr.ID {
		t.Fatalf("receipt attached to %s/%s", mod, owner)
	}
	if ev := e.audit.OfOutcome(authz.PaymentsTransferMarkReturned, audit.Success); len(ev) != 3 || ev[0].Reason != "Sipariş yok" {
		t.Fatalf("audit = %+v", ev)
	}
	// The decision without a proof is on record as denied (03 §6.2, BE-074).
	if ev := e.audit.OfOutcome(authz.PaymentsTransferMarkReturned, audit.Denied); len(ev) != 1 || ev[0].Reason != audit.ReasonReauthFailed ||
		*ev[0].ActorUserID != fin2.UserID {
		t.Fatalf("denied audit = %+v", ev)
	}
	if _, err := e.svc.RequestReturn(ctxOf(t, fin), fin, tr.ID, payments.ReturnRequest{Reason: "tekrar"}, good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("request on a returned transfer = %v", err)
	}

	// A rejection: back to unmatched, the request cleared; an ignored transfer may be returned.
	tr2 := e.transfer(t, acct, 5_000, "yanlış", false)
	if _, err := e.svc.RequestReturn(ctxOf(t, fin), fin, tr2.ID, payments.ReturnRequest{Reason: "x"}, good); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("no sender IBAN, no target = %v", err)
	}
	if _, err := e.svc.IgnoreTransfer(ctxOf(t, fin), fin, tr2.ID, "şüpheli"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RequestReturn(ctxOf(t, fin2), fin2, tr2.ID, payments.ReturnRequest{Reason: "Göndericiye", TargetIBAN: ibanOther}, good); err != nil {
		t.Fatal(err)
	}
	got, err = e.svc.DecideReturn(ctxOf(t, fin), fin, tr2.ID, payments.DecisionRejected, good)
	if err != nil || got.Status != payments.TransferUnmatched || got.ReturnRequestedBy != nil || got.ReturnTargetIBAN != nil || got.IgnoreReason != nil {
		t.Fatalf("rejected = %+v, %v", got, err)
	}
}

// TestUnmatchedDigest_Over24h (01 §7 payments.unmatched_digest): only the
// transfers unmatched for more than 24 hours are reported (count and the
// oldest), as an A event the finance admins receive in-app; nothing old →
// no event.
func TestUnmatchedDigest_Over24h(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	acct := e.activeAccount(t, ibanKoro, true)
	old := e.transfer(t, acct, 10, "eski", false)
	e.clk.Advance(2 * time.Hour)
	older := e.transfer(t, acct, 20, "eski 2", false)
	p := e.order(t, orderSpec{})
	matched := e.transfer(t, acct, 1_200_000, p.order.OrderNo, false)
	if _, err := e.match(t, matched, p, ""); err != nil {
		t.Fatal(err)
	}
	if sum, err := e.mod.RunDigest(ctx); err != nil || sum.Count != 0 || len(e.events(domainevent.TopicIncomingTransferUnmatched)) != 0 {
		t.Fatalf("young transfers reported: %+v, %v", sum, err)
	}
	e.clk.Advance(23 * time.Hour) // old: 25 h, older: 23 h
	e.transfer(t, acct, 30, "yeni", false)
	sum, err := e.mod.RunDigest(ctx)
	if err != nil || sum.Count != 1 || !sum.OldestAt.Equal(old.CreatedAt) {
		t.Fatalf("digest = %+v, %v (want only %s)", sum, err, old.ID)
	}
	evs := e.events(domainevent.TopicIncomingTransferUnmatched)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	pl, _ := domainevent.Decode[domainevent.IncomingTransferUnmatched](evs[0])
	if pl.Count != 1 || !pl.OldestAt.Equal(old.CreatedAt) {
		t.Fatalf("payload = %+v", pl)
	}
	e.clk.Advance(2 * time.Hour)
	if sum, _ := e.mod.RunDigest(ctx); sum.Count != 2 {
		t.Fatalf("after 2 more hours = %+v (older %s joins)", sum, older.ID)
	}
	e.deliver(t, domainevent.TopicIncomingTransferUnmatched)
	if got := e.sender.to("payments.unmatched_transfers", notify.ChannelInApp); !containsAll(got, id("finance_admin"), id("finance_admin_2")) || len(got) != 4 {
		t.Fatalf("digest notices to %v", got)
	}
	jobs := e.mod.Jobs()
	if len(jobs) != 1 || jobs[0].Name != payments.DigestJobName || jobs[0].Cron != "30 9 * * 1-5" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

// TestNotices_CollectionAccountApproved: the approval's security notice
// reaches every platform and finance admin in-app and by e-mail with the
// IBAN masked.
func TestNotices_CollectionAccountApproved(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createAccount(t, "finance_admin", ibanKoro)
	pad := actor(t, "platform_admin")
	if _, err := e.svc.DecideCollectionAccount(ctxOf(t, pad), pad, c.ID, payments.DecisionApproved, good); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicCollectionAccountApproved)
	for _, ch := range []notify.Channel{notify.ChannelInApp, notify.ChannelEmail} {
		got := e.sender.to("payments.collection_account_approved", ch)
		if len(got) != 4 || !containsAll(got, id("platform_admin"), id("platform_admin_2"), id("finance_admin"), id("finance_admin_2")) {
			t.Fatalf("%s to %v", ch, got)
		}
	}
	for _, m := range e.sender.sent {
		if m.Channel == notify.ChannelEmail && m.Data["iban_masked"] != "TR33 **** **** **** **** 8413 26" {
			t.Fatalf("e-mail data = %v", m.Data)
		}
		if m.Channel == notify.ChannelInApp && (len(m.Data) != 1 || m.Data["collection_account_id"] != c.ID.String()) {
			t.Fatalf("in-app data = %v", m.Data)
		}
	}
	n := len(e.sender.sent)
	e.deliver(t, domainevent.TopicCollectionAccountApproved)
	if len(e.sender.sent) != n {
		t.Fatal("a repeated task sent again")
	}
}

// TestPrivacy_RetainsFinancialRecords (01 §3.3): the staff references are
// exported (IDs, amounts, roles; never a sender's data) and retained.
func TestPrivacy_RetainsFinancialRecords(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	tr := e.transfer(t, acct, 1_200_000, p.order.OrderNo, true) // entered by finance_admin_2
	if _, err := e.match(t, tr, p, ""); err != nil {            // matched and captured by finance_admin
		t.Fatal(err)
	}
	sec, err := e.mod.Privacy.Export(t.Context(), id("finance_admin"))
	if err != nil || sec.Module != "payments" || len(sec.Items) != 2 {
		t.Fatalf("export = %+v, %v", sec, err)
	}
	for _, it := range sec.Items {
		for _, v := range it {
			if s, ok := v.(string); ok && (s == ibanSender || s == "ÖRNEK TEKNOLOJİ A.Ş.") {
				t.Fatalf("export carries the sender's data: %v", it)
			}
		}
	}
	res, err := e.mod.Privacy.Erase(t.Context(), id("finance_admin_2"))
	if err != nil || res.Retained != 1 || res.RetainedReason == "" {
		t.Fatalf("erase = %+v, %v", res, err)
	}
	if got, _ := e.store.GetTransfer(t.Context(), tr.ID, false); got.EnteredBy != id("finance_admin_2") {
		t.Fatal("erase changed a financial record")
	}
}
