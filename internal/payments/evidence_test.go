package payments_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/files/filestest"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/payments"
	"koro/internal/platform/civil"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// reject marks an upload rejected by the scan after its upload (infected:
// its attached row stays, BE-048).
func (e *env) reject(fid uuid.UUID) {
	e.files.set(fid, func(m *files.FileMeta) { m.ScanStatus = files.ScanInfected })
}

// stateOf is the state a response shows of file fid.
func (e *env) stateOf(t *testing.T, fid *uuid.UUID) files.State {
	t.Helper()
	st, err := e.svc.EvidenceStates(t.Context(), fid)
	if err != nil {
		t.Fatal(err)
	}
	return files.StateOf(st, fid)
}

// rejectedNotice delivers file.rejected of fid (purpose, attached to
// entity) to payments' subscription.
func (e *env) rejectedNotice(t *testing.T, fid uuid.UUID, purpose files.Purpose, entity uuid.UUID) {
	t.Helper()
	if err := filestest.Reject(t, e.mod.Subscribe, payments.NotifyRejectedEvidenceTaskName, fid, purpose, files.RejectInfected,
		"payments", entity); err != nil {
		t.Fatal(err)
	}
}

// wantReplaced checks the audit record of a replacement of old by next.
func wantReplaced(t *testing.T, e *env, act authz.Action, old, next uuid.UUID) audit.Event {
	t.Helper()
	for _, ev := range e.audit.Of(act) {
		if ev.Metadata["operation"] == "replace_rejected_file" {
			if ev.Metadata["replaced_file_id"] != old.String() || ev.Metadata["file_id"] != next.String() ||
				ev.Metadata["replaced_reason"] != string(files.RejectInfected) {
				t.Fatalf("replacement audit = %+v", ev.Metadata)
			}
			return ev
		}
	}
	t.Fatalf("no replace_rejected_file record of %s", act)
	return audit.Event{}
}

// TestReplaceEvidence_TransferReceipt (BE-154b): a transfer's receipt
// shows its state; only a rejected one is replaced, by the people who
// record transfers, with a transfer_receipt upload; the old file stays on
// record, the transfer's collection follows (ON UPDATE CASCADE), finance
// is told of the rejection and a late rejection of the replaced file is
// silent.
func TestReplaceEvidence_TransferReceipt(t *testing.T) {
	t.Parallel()
	w := &collectionLine{}
	e := newEnv(t, withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w }))
	_, acct := collectionDebt(t, e, w)
	tr := e.collectionSource(t, acct, 540_000)
	fin, fin2 := actor(t, "finance_admin"), actor(t, "finance_admin_2")
	c, err := e.svc.CreateCollection(ctxOf(t, fin), fin, payments.CollectionInput{IncomingTransferID: tr.ID, OrderLineID: w.line})
	if err != nil {
		t.Fatal(err)
	}
	old := *tr.ReceiptFileID
	if got := e.stateOf(t, tr.ReceiptFileID); got != files.StateClean {
		t.Fatalf("state = %q, want clean", got)
	}
	next := e.files.add(files.PurposeTransferReceipt)
	replace := func(who authz.Actor, transfer, file uuid.UUID) (payments.IncomingTransfer, error) {
		return e.svc.AdminReplaceTransferReceipt(ctxOf(t, who), who, transfer, file)
	}
	if _, err := replace(fin, tr.ID, next); !errors.Is(err, files.ErrNotRejected) {
		t.Fatalf("clean receipt: %v, want 409 files.not_rejected", err)
	}
	e.reject(old)
	if got := e.stateOf(t, tr.ReceiptFileID); got != files.StateRejected {
		t.Fatalf("state = %q, want rejected", got)
	}
	e.rejectedNotice(t, old, files.PurposeTransferReceipt, tr.ID)
	if got := e.sender.to("payments.transfer_receipt_rejected", notify.ChannelInApp); !containsAll(got, id("finance_admin"),
		id("finance_admin_2")) || len(got) != 2 {
		t.Fatalf("notice to %v, want the finance admins", got)
	}
	pad := actor(t, "platform_admin")
	if _, err := replace(pad, tr.ID, next); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform admin: %v, want 403", err)
	}
	if _, err := replace(fin, tr.ID, e.files.add(files.PurposeRefundReceipt)); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("wrong purpose: %v, want 422", err)
	}
	if _, err := replace(fin, tr.ID, uuid.Nil()); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("no file: %v, want 422", err)
	}
	if _, err := replace(fin, uuid.NewV7(), next); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown transfer: %v, want 404", err)
	}
	got, err := replace(fin2, tr.ID, next)
	if err != nil || got.ReceiptFileID == nil || *got.ReceiptFileID != next || got.Amount != tr.Amount ||
		got.Status != payments.TransferCollectionRequested {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	if mod, ent := e.files.owner(next); mod != "payments" || ent != tr.ID {
		t.Fatalf("new receipt attached to %s/%s", mod, ent)
	}
	if mod, ent := e.files.owner(old); mod != "payments" || ent != tr.ID {
		t.Fatalf("old receipt left the record: %s/%s", mod, ent)
	}
	wantReplaced(t, e, authz.PaymentsTransferCreate, old, next)
	if c, err = e.store.GetCollection(t.Context(), c.ID, false); err != nil || c.ReceiptFileID != next {
		t.Fatalf("collection receipt = %+v, %v (ON UPDATE CASCADE)", c, err)
	}
	if _, err := replace(fin, tr.ID, e.files.add(files.PurposeTransferReceipt)); !errors.Is(err, files.ErrNotRejected) {
		t.Fatalf("the replacement is clean: %v, want 409", err)
	}
	before := len(e.sender.sent)
	e.rejectedNotice(t, old, files.PurposeTransferReceipt, tr.ID)
	if len(e.sender.sent) != before {
		t.Fatal("a late rejection of the replaced receipt sent a notice")
	}
	plain := e.transfer(t, acct, 1_000, "tanımsız", false)
	if _, err := replace(fin, plain.ID, e.files.add(files.PurposeTransferReceipt)); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("a transfer without a receipt: %v, want 409 payments.invalid_state", err)
	}
}

// TestReplaceEvidence_ReturnReceipt: the return's receipt is replaced by
// the people of its receipt step with their proof (R), only on a returned
// transfer; the return's bank reference and eyes stay.
func TestReplaceEvidence_ReturnReceipt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tr := e.requestedReturn(t)
	fin, fin2 := actor(t, "finance_admin"), actor(t, "finance_admin_2")
	replace := func(who authz.Actor, file uuid.UUID, proof identity.ReauthProof) (payments.IncomingTransfer, error) {
		return e.svc.AdminReplaceReturnReceipt(ctxOf(t, who), who, tr.ID, file, proof)
	}
	if _, err := replace(fin, e.files.add(files.PurposeRefundReceipt), good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("not returned yet: %v, want 409", err)
	}
	if _, err := e.svc.DecideReturn(ctxOf(t, fin2), fin2, tr.ID, payments.DecisionApproved, good); err != nil {
		t.Fatal(err)
	}
	old := e.files.add(files.PurposeRefundReceipt)
	done, err := e.svc.MarkReturned(ctxOf(t, fin), fin, tr.ID, payments.ReturnReceipt{BankReference: "EFT-R1", ReceiptFileID: old}, good)
	if err != nil {
		t.Fatal(err)
	}
	e.reject(old)
	e.rejectedNotice(t, old, files.PurposeRefundReceipt, tr.ID)
	if got := e.sender.to("payments.refund_receipt_rejected", notify.ChannelInApp); len(got) != 2 {
		t.Fatalf("notice to %v, want the two finance admins", got)
	}
	next := e.files.add(files.PurposeRefundReceipt)
	if _, err := replace(fin, next, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("no proof: %v, want 403 identity.reauth_required", err)
	}
	if _, err := replace(fin, e.files.add(files.PurposeTransferReceipt), good); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("wrong purpose: %v, want 422", err)
	}
	got, err := replace(fin2, next, good)
	if err != nil || got.ReturnReceiptFileID == nil || *got.ReturnReceiptFileID != next {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	if *got.ReturnBankReference != *done.ReturnBankReference || *got.ReturnApprovedBy != *done.ReturnApprovedBy ||
		got.Status != payments.TransferReturned {
		t.Fatalf("the return changed: %+v", got)
	}
	ev := wantReplaced(t, e, authz.PaymentsTransferMarkReturned, old, next)
	if ev.Metadata[audit.MetaReauth] == nil {
		t.Fatalf("no reauth kind in %+v", ev.Metadata)
	}
}

// TestReplaceEvidence_PaymentEvidence: a model C direct payment's evidence
// is replaced by the people who confirm it; a matched payment has none.
func TestReplaceEvidence_PaymentEvidence(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{modelC: true})
	fin := actor(t, "finance_admin")
	old := e.files.add(files.PurposeTransferReceipt)
	ctx := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{Scope: "direct", Value: "evidence-1", Status: 200})
	d, err := e.svc.ConfirmDirectPayment(ctx, fin, payments.DirectInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo,
		EvidenceFileID: old})
	if err != nil {
		t.Fatal(err)
	}
	pay := d.Payment.PaymentID
	e.reject(old)
	e.rejectedNotice(t, old, files.PurposeTransferReceipt, pay)
	if got := e.sender.to("payments.transfer_receipt_rejected", notify.ChannelInApp); len(got) != 2 {
		t.Fatalf("notice to %v, want the two finance admins", got)
	}
	next := e.files.add(files.PurposeTransferReceipt)
	cma := actor(t, "company_admin")
	if _, err := e.svc.AdminReplacePaymentEvidence(ctxOf(t, cma), cma, pay, next); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("buyer: %v, want 403", err)
	}
	got, err := e.svc.AdminReplacePaymentEvidence(ctxOf(t, fin), fin, pay, next)
	if err != nil || got.EvidenceFileID == nil || *got.EvidenceFileID != next || got.Amount == 0 {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	wantReplaced(t, e, authz.PaymentsTransferMatch, old, next)
	matched := e.paid(t, acct, orderSpec{}, 0)
	if _, err := e.svc.AdminReplacePaymentEvidence(ctxOf(t, fin), fin, matched.payment.ID,
		e.files.add(files.PurposeTransferReceipt)); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("a matched payment: %v, want 409", err)
	}
}

// TestReplaceEvidence_RefundReceipt: a completed refund's receipt is
// replaced by the people who complete refunds; the refund, its bank
// reference and the ledger do not change; an approved refund has none.
func TestReplaceEvidence_RefundReceipt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 100_000))
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminReplaceRefundReceipt(ctxOf(t, fin), fin, r.ID,
		e.files.add(files.PurposeRefundReceipt)); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("approved refund: %v, want 409", err)
	}
	res, err := e.completeAs(t, "finance_admin", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := *res.Refund.ReceiptFileID
	txns := len(e.refundTxns(t))
	e.reject(old)
	e.rejectedNotice(t, old, files.PurposeRefundReceipt, r.ID)
	if got := e.sender.to("payments.refund_receipt_rejected", notify.ChannelInApp); len(got) != 2 {
		t.Fatalf("notice to %v, want the two finance admins", got)
	}
	next := e.files.add(files.PurposeRefundReceipt)
	sup := actor(t, "support")
	if _, err := e.svc.AdminReplaceRefundReceipt(ctxOf(t, sup), sup, r.ID, next); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("support: %v, want 403", err)
	}
	got, err := e.svc.AdminReplaceRefundReceipt(ctxOf(t, fin), fin, r.ID, next)
	if err != nil || got.ReceiptFileID == nil || *got.ReceiptFileID != next || *got.BankReference != *res.Refund.BankReference ||
		got.Status != payments.RefundStatusCompleted {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	if len(e.refundTxns(t)) != txns {
		t.Fatal("the replacement posted to the ledger")
	}
	wantReplaced(t, e, authz.PaymentsRefundComplete, old, next)
}

// TestReplaceEvidence_RefundRequestEvidence: the buying company's admin
// replaces a waiting request's rejected evidence with an upload of the
// company; the seller, finance and another company cannot; the buyer and
// finance are told; a decided request keeps its file and is silent.
func TestReplaceEvidence_RefundRequestEvidence(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	old := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))
	v, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 5_000,
		func(in *payments.RefundRequestInput) { in.EvidenceFileID = &old })
	if err != nil {
		t.Fatal(err)
	}
	if v.EvidenceState != files.StateClean {
		t.Fatalf("view state = %q, want clean", v.EvidenceState)
	}
	req := v.Request.ID
	cma := actor(t, "company_admin")
	replace := func(who authz.Actor, file uuid.UUID) (payments.RefundRequestView, error) {
		return e.svc.ReplaceRefundRequestEvidence(ctxOf(t, who), who, req, file)
	}
	next := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))
	if _, err := replace(cma, next); !errors.Is(err, files.ErrNotRejected) {
		t.Fatalf("clean evidence: %v, want 409", err)
	}
	e.reject(old)
	e.rejectedNotice(t, old, files.PurposeRefundRequestEvidence, req)
	if got := e.sender.to("payments.refund_request_evidence_rejected", notify.ChannelInApp); !containsAll(got, id("company_admin"),
		id("finance_admin"), id("finance_admin_2")) {
		t.Fatalf("notice to %v, want the buyer's admins and finance", got)
	}
	for persona, want := range map[string]error{"club_admin": authz.ErrForbidden, "finance_admin": authz.ErrForbidden,
		"company_member": authz.ErrForbidden, "other_company_member": authz.ErrNotFound} {
		if _, err := replace(actor(t, persona), next); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", persona, err, want)
		}
	}
	if _, err := replace(cma, e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_b"))); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("another company's upload: %v, want 422", err)
	}
	got, err := replace(cma, next)
	if err != nil || got.Request.EvidenceFileID == nil || *got.Request.EvidenceFileID != next || got.EvidenceState != files.StateClean ||
		got.Request.Status != payments.RequestOpen {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	wantReplaced(t, e, authz.PaymentsRefundRequestCreate, old, next)
	if _, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, req); err != nil {
		t.Fatal(err)
	}
	e.reject(next)
	before := len(e.sender.sent)
	e.rejectedNotice(t, next, files.PurposeRefundRequestEvidence, req)
	if len(e.sender.sent) != before {
		t.Fatal("a decided request's rejection sent a notice")
	}
	if _, err := replace(cma, e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("withdrawn request: %v, want 409", err)
	}
}

// TestReplaceEvidence_StoreSlots: ReplaceEvidence writes only the slot's
// file, only from the file it holds and in the slot's state (store
// contract on the memory store; the PostgreSQL run adds the guard).
func TestReplaceEvidence_StoreSlots(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	fin := actor(t, "finance_admin")
	receipt := e.files.add(files.PurposeTransferReceipt)
	tr, err := e.svc.CreateTransfer(ctxOf(t, fin), fin, payments.TransferInput{CollectionAccountID: acct.ID,
		ValueDate: civil.From(e.clk.Now()), Amount: 5_000, Description: "x", BankReference: "SLOT-1", ReceiptFileID: &receipt})
	if err != nil {
		t.Fatal(err)
	}
	ctx, next := t.Context(), uuid.NewV7()
	if err := e.store.ReplaceEvidence(ctx, payments.SlotTransferReceipt, tr.ID, uuid.NewV7(), next); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("another current file: %v, want ErrInvalidState", err)
	}
	if err := e.store.ReplaceEvidence(ctx, payments.SlotReturnReceipt, tr.ID, receipt, next); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("not returned: %v, want ErrInvalidState", err)
	}
	if err := e.store.ReplaceEvidence(ctx, payments.SlotRefundReceipt, uuid.NewV7(), receipt, next); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown refund: %v, want ErrNotFound", err)
	}
	if err := e.store.ReplaceEvidence(ctx, payments.SlotTransferReceipt, tr.ID, receipt, next); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.GetTransfer(ctx, tr.ID, false)
	if err != nil || *got.ReceiptFileID != next || got.Status != tr.Status || got.BankReference != tr.BankReference {
		t.Fatalf("after = %+v, %v", got, err)
	}
}
