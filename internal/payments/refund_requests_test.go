package payments_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// explanation is a buyer's explanation of the tests (10–2000 characters).
const explanation = "Stant kurulmadı; etkinlik günü alan boştu."

// askRefund opens a refund request of line of po's order (company_a) as
// persona under a fresh Idempotency-Key; mod edits the body.
func (e *env) askRefund(t *testing.T, persona string, orderID, line uuid.UUID, amount int64,
	mod func(*payments.RefundRequestInput)) (payments.RefundRequestView, error) {
	t.Helper()
	a := actor(t, persona)
	in := payments.RefundRequestInput{OrderLineID: line, Amount: amount, Reason: payments.RefundReasonNotFulfilled,
		Explanation: explanation, TargetIBAN: ibanOther}
	if mod != nil {
		mod(&in)
	}
	ctx := idempotency.WithKey(ctxOf(t, a), idempotency.Key{Scope: a.UserID.String() + ":POST refund-requests",
		Value: uuid.NewV7().String(), Status: 201})
	return e.svc.CreateRefundRequest(ctx, a, id("company_a"), orderID, in)
}

// acceptAs accepts request id as persona under a fresh Idempotency-Key.
func (e *env) acceptAs(t *testing.T, persona string, reqID uuid.UUID, in payments.AcceptInput) (payments.RefundRequestView, error) {
	t.Helper()
	a := actor(t, persona)
	ctx := idempotency.WithKey(ctxOf(t, a), idempotency.Key{Scope: a.UserID.String() + ":POST accept",
		Value: uuid.NewV7().String(), Status: 200})
	return e.svc.AcceptRefundRequest(ctx, a, reqID, in)
}

// setLine edits a line of the orders store directly (its delivery state).
func (e *env) setLine(t *testing.T, lineID uuid.UUID, edit func(*orders.Line)) {
	t.Helper()
	ctx := t.Context()
	l, err := e.ordStore.GetLine(ctx, lineID, false)
	if err != nil {
		t.Fatal(err)
	}
	from := l.FulfillmentStatus
	edit(&l)
	if _, err := e.ordStore.UpdateLine(ctx, l, from); err != nil {
		t.Fatal(err)
	}
}

// deliveredAt marks a line fulfilled at the time given.
func (e *env) deliveredAt(t *testing.T, lineID uuid.UUID, at time.Time) {
	t.Helper()
	via := orders.ViaBuyerConfirmed
	e.setLine(t, lineID, func(l *orders.Line) {
		l.FulfillmentStatus, l.FulfillmentMarkedAt, l.FulfilledAt, l.FulfilledVia = orders.FulfillFulfilled, &at, &at, &via
	})
}

// runRequestNotices runs every queued refund request notice task (twice:
// the dedupe keys make the second run a no-op).
func (e *env) runRequestNotices(t *testing.T) {
	t.Helper()
	for _, task := range e.queue.Tasks() {
		if task.Type != payments.NotifyRefundRequestTaskName {
			continue
		}
		for _, h := range e.mod.Tasks() {
			if h.Name != task.Type {
				continue
			}
			for range 2 {
				if err := h.Handle(t.Context(), task.Payload); err != nil {
					t.Fatalf("%s: %v", h.Name, err)
				}
			}
		}
	}
	e.queue.Reset()
}

// TestRefundRequest_AcceptOpensFourEyesRefund (BE-132, Q-26): the buyer's
// admin asks for a refund of a paid line; finance takes it into review and
// accepts it, which opens a refund of the line and the amount in the same
// transaction (requested by the accepting admin, to the buyer's IBAN, for
// the buyer's reason, from the order's payment); the four eyes and the EFT
// follow unchanged; the buyer's read shows the refund's status; every step
// is audited and the masked IBAN only is recorded.
func TestRefundRequest_AcceptOpensFourEyesRefund(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	line := po.lines[0]
	v, err := e.askRefund(t, "company_admin", po.order.ID, line.ID, 300_000, nil)
	r := v.Request
	if err != nil || r.Status != payments.RequestOpen || r.OrgID != id("company_a") || r.SellerOrgID != id("club_a") ||
		r.OrderNo != po.order.OrderNo || r.LineTitle != "Stant" || r.TargetIBAN != ibanOther || r.Amount != 300_000 ||
		r.RequestedBy != id("company_admin") || v.Audience != payments.AudienceBuyer || v.BuyerName == "" || v.SellerName == "" {
		t.Fatalf("create = %+v, %v", v, err)
	}
	ev := e.audit.Of(authz.PaymentsRefundRequestCreate)
	if len(ev) != 1 || ev[0].Metadata["target_iban_masked"] == ibanOther || ev[0].Metadata["amount"] != int64(300_000) ||
		ev[0].OrgID == nil || *ev[0].OrgID != id("company_a") {
		t.Fatalf("create audit = %+v", ev)
	}
	fin := actor(t, "finance_admin")
	rv, err := e.svc.ReviewRefundRequest(ctxOf(t, fin), fin, r.ID)
	if err != nil || rv.Request.Status != payments.RequestInReview || rv.Request.ReviewedBy == nil || *rv.Request.ReviewedBy != fin.UserID {
		t.Fatalf("review = %+v, %v", rv, err)
	}
	if _, err := e.svc.ReviewRefundRequest(ctxOf(t, fin), fin, r.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("second review = %v", err)
	}
	av, err := e.acceptAs(t, "finance_admin", r.ID, payments.AcceptInput{})
	if err != nil || av.Request.Status != payments.RequestAccepted || av.Request.RefundID == nil || av.Refund == nil ||
		av.Request.DecidedBy == nil || *av.Request.DecidedBy != fin.UserID {
		t.Fatalf("accept = %+v, %v", av, err)
	}
	ref := *av.Refund
	if ref.Status != payments.RefundStatusRequested || ref.RequestedBy != fin.UserID || ref.Amount != 300_000 ||
		ref.OrderLineID == nil || *ref.OrderLineID != line.ID || ref.PaymentID != po.payment.ID || ref.TargetIBAN == nil ||
		*ref.TargetIBAN != ibanOther || ref.Reason != payments.RefundReasonNotFulfilled {
		t.Fatalf("refund = %+v", ref)
	}
	if rc := e.audit.Of(authz.PaymentsRefundCreate); len(rc) != 1 || rc[0].Metadata["via"] != "refund_request" ||
		rc[0].Metadata["refund_request_id"] != r.ID.String() {
		t.Fatalf("refund audit = %+v", rc)
	}
	if acc := e.audit.Of(authz.PaymentsRefundRequestAccept); len(acc) != 1 || acc[0].Metadata["refund_id"] != ref.ID.String() {
		t.Fatalf("accept audit = %+v", acc)
	}
	// The four eyes of the refund are unchanged: its requester never approves it.
	if _, err := e.approve(t, "finance_admin", ref.ID); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("approve by the accepting admin = %v", err)
	}
	if _, err := e.approve(t, "finance_admin_2", ref.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.completeAs(t, "finance_admin", ref.ID); err != nil {
		t.Fatal(err)
	}
	// A decided request is final.
	if _, err := e.acceptAs(t, "finance_admin_2", r.ID, payments.AcceptInput{}); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("second accept = %v", err)
	}
	buyer := actor(t, "company_admin")
	got, err := e.svc.GetRefundRequest(ctxOf(t, buyer), buyer, r.ID)
	if err != nil || got.Audience != payments.AudienceBuyer || got.Refund == nil || got.Refund.Status != payments.RefundStatusCompleted {
		t.Fatalf("buyer's read = %+v, %v", got, err)
	}
	if l, _ := e.ordStore.GetLine(t.Context(), line.ID, false); l.RefundedAmount != 300_000 {
		t.Fatalf("line refunded = %d", l.RefundedAmount)
	}
}

// TestRefundRequest_AmountWithinWhatKoroHolds: a request asks at most what
// Koro holds of the line less the refunds (completed) and the open ones
// (requested, approved), in kuruş; a line refunded in full has nothing
// left; the acceptance checks again (a refund opened meanwhile).
func TestRefundRequest_AmountWithinWhatKoroHolds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	line := po.lines[0].ID
	if _, err := e.askRefund(t, "company_admin", po.order.ID, line, 1_200_001, nil); !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("beyond the line = %v", err)
	}
	if problemtestStatus(t, payments.ErrRefundExceedsPaid) != 422 {
		t.Fatal("orders.refund_exceeds_paid is not a 422")
	}
	open := e.approved(t, lineRefund(po.payment.ID, line, 200_000)) // an open (approved) refund holds 200,000
	if _, err := e.askRefund(t, "company_admin", po.order.ID, line, 1_000_001, nil); !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("beyond the line less the open refund = %v", err)
	}
	if _, err := e.completeAs(t, "finance_admin", open.ID); err != nil {
		t.Fatal(err)
	}
	v, err := e.askRefund(t, "company_admin", po.order.ID, line, 1_000_000, nil)
	if err != nil {
		t.Fatalf("exactly what is left = %v", err)
	}
	// Finance refunds part of the line by hand meanwhile: the acceptance
	// finds less than the request asks.
	if _, err := e.request(t, "finance_admin_2", lineRefund(po.payment.ID, line, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.acceptAs(t, "finance_admin", v.Request.ID, payments.AcceptInput{}); problemtestStatus(t, err) != 422 {
		t.Fatalf("accept beyond what is left now = %v", err) // no payment holds it any more
	}
	if _, err := e.acceptAs(t, "finance_admin", v.Request.ID, payments.AcceptInput{PaymentID: &po.payment.ID}); !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("accept from the payment beyond what is left now = %v", err)
	}
	if got, _ := e.store.GetRefundRequest(t.Context(), v.Request.ID, false); got.Status != payments.RequestOpen {
		t.Fatalf("after the refused acceptance: %s", got.Status)
	}
}

func problemtestStatus(t *testing.T, err error) int {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) {
		t.Fatalf("%v is not a problem", err)
	}
	return pe.Status()
}

// TestRefundRequest_Eligibility: which orders and lines a buyer may ask
// (an unpaid order 409 orders.order_not_paid; a cancelled line 409
// orders.invalid_state; one waiting request per line 409
// payments.refund_request_open; a delivered line within the window only,
// 409 payments.refund_request_window_closed after it; an undelivered line
// any time) and the refund options that say so; another org's order is
// 404; only the company admin asks (the recruiter reads).
func TestRefundRequest_Eligibility(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	unpaid := e.order(t, orderSpec{})
	if _, err := e.askRefund(t, "company_admin", unpaid.order.ID, unpaid.lines[0].ID, 1, nil); !errors.Is(err, orders.ErrOrderNotPaid) {
		t.Fatalf("unpaid order = %v", err)
	}
	po := e.paid(t, acct, orderSpec{platformLine: true}, 0)
	booth, platform := po.lines[0].ID, po.lines[1].ID
	cma, rec := actor(t, "company_admin"), actor(t, "company_member")
	opts, err := e.svc.RefundOptions(ctxOf(t, rec), rec, id("company_a"), po.order.ID)
	if err != nil || len(opts.Lines) != 2 || opts.Lines[0].Refundable != 1_200_000 || opts.Lines[0].Deadline != nil ||
		opts.Lines[0].Blocked != "" || opts.Lines[1].Refundable != 300_000 || opts.Lines[1].Deadline == nil {
		t.Fatalf("options (the platform line is delivered at payment) = %+v, %v", opts, err)
	}
	if _, err := e.askRefund(t, "company_member", po.order.ID, booth, 1, nil); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("the recruiter asks = %v", err)
	}
	other := actor(t, "other_company_member") // company_b's admin
	if _, err := e.svc.CreateRefundRequest(ctxOf(t, other), other, id("company_b"), po.order.ID, payments.RefundRequestInput{
		OrderLineID: booth, Amount: 1, Reason: payments.RefundReasonOther, Explanation: explanation, TargetIBAN: ibanOther,
	}); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("another company's order = %v", err)
	}
	if _, err := e.svc.RefundOptions(ctxOf(t, cma), cma, id("company_a"), uuid.NewV7()); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown order = %v", err)
	}
	// A line of another order is not this order's.
	if _, err := e.askRefund(t, "company_admin", po.order.ID, unpaid.lines[0].ID, 1, nil); problemtestStatus(t, err) != 422 {
		t.Fatalf("another order's line = %v", err)
	}
	// One waiting request per line.
	first, err := e.askRefund(t, "company_admin", po.order.ID, booth, 100_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.askRefund(t, "company_admin", po.order.ID, booth, 1, nil); !errors.Is(err, payments.ErrRefundRequestOpen) {
		t.Fatalf("second waiting request = %v", err)
	}
	if opts, _ = e.svc.RefundOptions(ctxOf(t, cma), cma, id("company_a"), po.order.ID); opts.Lines[0].Blocked != payments.BlockedOpenRequest ||
		opts.Lines[0].OpenRequest == nil || *opts.Lines[0].OpenRequest != first.Request.ID || len(opts.Requests) != 1 {
		t.Fatalf("options with a waiting request = %+v", opts)
	}
	// The window: the platform line was delivered at payment (now); 30 days on it closes.
	e.clk.Advance(payments.DefaultRefundRequestWindow - time.Minute)
	if _, err := e.askRefund(t, "company_admin", po.order.ID, platform, 1_000, nil); err != nil {
		t.Fatalf("inside the window = %v", err)
	}
	po2 := e.paid(t, acct, orderSpec{}, 0)
	e.deliveredAt(t, po2.lines[0].ID, e.clk.Now().Add(-payments.DefaultRefundRequestWindow))
	if _, err := e.askRefund(t, "company_admin", po2.order.ID, po2.lines[0].ID, 1, nil); !errors.Is(err, payments.ErrRefundRequestWindowClosed) {
		t.Fatalf("after the window = %v", err)
	}
	// A cancelled line is finance's refund due, never a request.
	po3 := e.paid(t, acct, orderSpec{}, 0)
	e.setLine(t, po3.lines[0].ID, func(l *orders.Line) { l.FulfillmentStatus = orders.FulfillCancelled })
	if _, err := e.askRefund(t, "company_admin", po3.order.ID, po3.lines[0].ID, 1, nil); !errors.Is(err, orders.ErrInvalidState) {
		t.Fatalf("cancelled line = %v", err)
	}
	if opts, _ = e.svc.RefundOptions(ctxOf(t, cma), cma, id("company_a"), po3.order.ID); opts.Lines[0].Blocked != payments.BlockedLineState ||
		opts.Lines[0].Refundable != 0 {
		t.Fatalf("options of a cancelled line = %+v", opts)
	}
	// The body rules (422).
	for name, mod := range map[string]func(*payments.RefundRequestInput){
		"no line":          func(in *payments.RefundRequestInput) { in.OrderLineID = uuid.Nil() },
		"zero amount":      func(in *payments.RefundRequestInput) { in.Amount = 0 },
		"overpayment":      func(in *payments.RefundRequestInput) { in.Reason = payments.RefundReasonOverpayment },
		"short story":      func(in *payments.RefundRequestInput) { in.Explanation = "kısa" },
		"no IBAN":          func(in *payments.RefundRequestInput) { in.TargetIBAN = "" },
		"bad IBAN":         func(in *payments.RefundRequestInput) { in.TargetIBAN = "TR000000000000000000000000" },
		"nil evidence ID":  func(in *payments.RefundRequestInput) { nilID := uuid.Nil(); in.EvidenceFileID = &nilID },
		"bidi explanation": func(in *payments.RefundRequestInput) { in.Explanation = "Stant kurulmadı \u202e gerçekten" },
	} {
		if _, err := e.askRefund(t, "company_admin", po2.order.ID, po2.lines[0].ID, 1, mod); problemtestStatus(t, err) != 422 {
			t.Errorf("%s = %v, want 422", name, err)
		}
	}
}

// TestRefundRequest_IdempotencyReplay: the same key and body answer the
// same request (one row, one audit record, the evidence attached once);
// another body with the key is refused.
func TestRefundRequest_IdempotencyReplay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	ev := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))
	cma := actor(t, "company_admin")
	ctx := idempotency.WithKey(ctxOf(t, cma), idempotency.Key{Scope: cma.UserID.String() + ":POST rr", Value: "rr-1", Status: 201})
	in := payments.RefundRequestInput{OrderLineID: po.lines[0].ID, Amount: 5_000, Reason: payments.RefundReasonOther,
		Explanation: explanation, TargetIBAN: ibanOther, EvidenceFileID: &ev}
	first, err := e.svc.CreateRefundRequest(ctx, cma, id("company_a"), po.order.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateRefundRequest(ctx, cma, id("company_a"), po.order.ID, in)
	if err != nil || again.Request.ID != first.Request.ID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := len(e.audit.Of(authz.PaymentsRefundRequestCreate)); n != 1 {
		t.Fatalf("%d audit records after the replay", n)
	}
	if mod, ent := e.files.owner(ev); mod != "payments" || ent != first.Request.ID {
		t.Fatalf("evidence owner = %s %s", mod, ent)
	}
	other := in
	other.Amount = 6_000
	if _, err := e.svc.CreateRefundRequest(ctx, cma, id("company_a"), po.order.ID, other); !errors.Is(err, idempotency.ErrKeyReused) {
		t.Fatalf("same key, other body = %v", err)
	}
}

// TestRefundRequest_Evidence (KVKK): the evidence is a completed upload of
// the buying company (another org's file, another purpose and an unknown
// one read alike: 422; an unfinished one 422; an infected one 409); the
// parties download it through a 5-minute URL, every download audited;
// the recruiter and the seller's career staff do not; a request without
// evidence has none (404).
func TestRefundRequest_Evidence(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	line := po.lines[0].ID
	withFile := func(f uuid.UUID) func(*payments.RefundRequestInput) {
		return func(in *payments.RefundRequestInput) { in.EvidenceFileID = &f }
	}
	foreign := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_b"))
	wrong := e.files.addOrg(files.PurposeDeliveryEvidence, id("company_a"))
	unfinished := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))
	e.files.set(unfinished, func(m *files.FileMeta) { m.Status = files.StatusUploading })
	infected := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))
	e.files.set(infected, func(m *files.FileMeta) { m.ScanStatus = files.ScanInfected })
	for name, f := range map[string]uuid.UUID{"another org's": foreign, "another purpose": wrong, "unknown": uuid.NewV7(), "unfinished": unfinished} {
		if _, err := e.askRefund(t, "company_admin", po.order.ID, line, 1, withFile(f)); problemtestStatus(t, err) != 422 {
			t.Errorf("%s file = %v, want 422", name, err)
		}
	}
	if _, err := e.askRefund(t, "company_admin", po.order.ID, line, 1, withFile(infected)); !errors.Is(err, files.ErrNotClean) {
		t.Fatalf("infected file = %v", err)
	}
	if n := len(e.mem.RefundRequests()); n != 0 {
		t.Fatalf("%d requests after the refusals", n)
	}
	ev := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))
	v, err := e.askRefund(t, "company_admin", po.order.ID, line, 1, withFile(ev))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"company_admin", "club_admin", "finance_admin", "platform_admin"} {
		a := actor(t, p)
		dl, err := e.svc.RefundRequestEvidence(ctxOf(t, a), a, v.Request.ID)
		if err != nil || dl.URL == "" {
			t.Errorf("%s download = %+v, %v", p, dl, err)
		}
	}
	if n := len(e.audit.Of(authz.PaymentsRefundRequestReadEvidence)); n != 4 {
		t.Fatalf("%d download audit records, want 4", n)
	}
	for p, want := range map[string]error{"company_member": authz.ErrForbidden, "support": authz.ErrForbidden,
		"club_member": authz.ErrForbidden, "other_company_member": authz.ErrNotFound, "student": authz.ErrNotFound} {
		a := actor(t, p)
		if _, err := e.svc.RefundRequestEvidence(ctxOf(t, a), a, v.Request.ID); !errors.Is(err, want) {
			t.Errorf("%s download = %v, want %v", p, err, want)
		}
	}
	plain, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[1].ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	cma := actor(t, "company_admin")
	if _, err := e.svc.RefundRequestEvidence(ctxOf(t, cma), cma, plain.Request.ID); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("no evidence = %v", err)
	}
}

// TestRefundRequest_WithdrawRejectReview: the buyer withdraws a waiting
// request (then nothing decides it); finance rejects one with a reason
// (422 without; the buyer reads it); a decided request is never withdrawn
// or decided again; only an open request is taken into review.
func TestRefundRequest_WithdrawRejectReview(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	cma, fin := actor(t, "company_admin"), actor(t, "finance_admin")
	w, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 10_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.WithdrawRefundRequest(ctxOf(t, actor(t, "company_member")), actor(t, "company_member"), w.Request.ID); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("the recruiter withdraws = %v", err)
	}
	got, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, w.Request.ID)
	if err != nil || got.Request.Status != payments.RequestWithdrawn || got.Request.WithdrawnBy == nil {
		t.Fatalf("withdraw = %+v, %v", got, err)
	}
	if _, err := e.acceptAs(t, "finance_admin", w.Request.ID, payments.AcceptInput{}); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("accept a withdrawn request = %v", err)
	}
	if _, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, w.Request.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("withdraw twice = %v", err)
	}
	// The line is free again once its request is withdrawn.
	r, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 10_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RejectRefundRequest(ctxOf(t, fin), fin, r.Request.ID, " "); problemtestStatus(t, err) != 422 {
		t.Fatalf("reject without a reason = %v", err)
	}
	if _, err := e.svc.RejectRefundRequest(ctxOf(t, cma), cma, r.Request.ID, "Teslim belgelendi"); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("the buyer rejects = %v", err)
	}
	rej, err := e.svc.RejectRefundRequest(ctxOf(t, fin), fin, r.Request.ID, "Teslim tutanağı imzalı")
	if err != nil || rej.Request.Status != payments.RequestRejected || rej.Request.RejectionReason == nil {
		t.Fatalf("reject = %+v, %v", rej, err)
	}
	if ev := e.audit.Of(authz.PaymentsRefundRequestReject); len(ev) != 1 || ev[0].Reason != "Teslim tutanağı imzalı" {
		t.Fatalf("reject audit = %+v", ev)
	}
	if _, err := e.svc.ReviewRefundRequest(ctxOf(t, fin), fin, r.Request.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("review a rejected request = %v", err)
	}
	if _, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, r.Request.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("withdraw a rejected request = %v", err)
	}
	if got, err := e.svc.GetRefundRequest(ctxOf(t, cma), cma, r.Request.ID); err != nil || got.Request.RejectionReason == nil ||
		*got.Request.RejectionReason != "Teslim tutanağı imzalı" {
		t.Fatalf("the buyer reads = %+v, %v", got, err)
	}
	// The buyer withdraws a request in review too.
	ir, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[1].ID, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReviewRefundRequest(ctxOf(t, fin), fin, ir.Request.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, ir.Request.ID); err != nil || got.Request.Status != payments.RequestWithdrawn ||
		got.Request.ReviewedBy == nil {
		t.Fatalf("withdraw in review = %+v, %v", got, err)
	}
}

// TestRefundRequest_RequesterNeverDecides: a staff member who is also an
// admin of the buying company asks for a refund; they never review,
// accept or reject it (403 authz.four_eyes_violation); another admin does.
func TestRefundRequest_RequesterNeverDecides(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	both := actor(t, "finance_admin")
	both.Memberships = append(both.Memberships, authz.Membership{OrgID: id("company_a"), OrgKind: authz.OrgCompany,
		Role: authz.MemberCompanyAdmin})
	ctx := idempotency.WithKey(ctxOf(t, both), idempotency.Key{Scope: "x", Value: "y", Status: 201})
	v, err := e.svc.CreateRefundRequest(ctx, both, id("company_a"), po.order.ID, payments.RefundRequestInput{
		OrderLineID: po.lines[0].ID, Amount: 1_000, Reason: payments.RefundReasonOther, Explanation: explanation, TargetIBAN: ibanOther})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReviewRefundRequest(ctxOf(t, both), both, v.Request.ID); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("review own = %v", err)
	}
	if _, err := e.svc.AcceptRefundRequest(ctxOf(t, both), both, v.Request.ID, payments.AcceptInput{}); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("accept own = %v", err)
	}
	if _, err := e.svc.RejectRefundRequest(ctxOf(t, both), both, v.Request.ID, "Kendi talebim"); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("reject own = %v", err)
	}
	// The store refuses it too (refund_requests_decision_eyes_chk).
	r := v.Request
	now, by, why := e.clk.Now(), r.RequestedBy, "Kendi talebim"
	r.Status, r.DecidedBy, r.DecidedAt, r.RejectionReason = payments.RequestRejected, &by, &now, &why
	if _, err := e.store.UpdateRefundRequest(t.Context(), r, payments.RequestOpen); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("store decision by the requester = %v", err)
	}
	if _, err := e.acceptAs(t, "finance_admin_2", v.Request.ID, payments.AcceptInput{}); err != nil {
		t.Fatalf("another admin accepts = %v", err)
	}
}

// TestRefundRequest_AcceptPaymentAndOverrides: the refund draws from the
// payment finance names (one of the request's order; 422 otherwise) or
// the order's first captured transfer that holds the amount (422 when none
// does); finance may correct the IBAN and the refund's reason (never an
// overpayment); a line in an open payout is 409 and the request waits.
func TestRefundRequest_AcceptPaymentAndOverrides(t *testing.T) {
	t.Parallel()
	held := &openPayout{}
	e := newEnv(t, withLedgerLines(func(l payments.LedgerLines) payments.LedgerLines { held.LedgerLines = l; return held }))
	acct := e.activeAccount(t, ibanKoro, true)
	po := e.paid(t, acct, orderSpec{}, 0)
	other := e.paid(t, acct, orderSpec{}, 0)
	v, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 50_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]payments.AcceptInput{
		"another order's payment": {PaymentID: &other.payment.ID},
		"unknown payment":         {PaymentID: new(uuid.NewV7())},
		"overpayment reason":      {RefundReason: payments.RefundReasonOverpayment},
		"bad IBAN":                {TargetIBAN: "TR12"},
	} {
		if _, err := e.acceptAs(t, "finance_admin", v.Request.ID, in); problemtestStatus(t, err) != 422 {
			t.Errorf("%s = %v, want 422", name, err)
		}
	}
	line := po.lines[0].ID
	held.line = &line
	if _, err := e.acceptAs(t, "finance_admin", v.Request.ID, payments.AcceptInput{}); !errors.Is(err, payments.ErrLineInPayout) {
		t.Fatalf("line in an open payout = %v", err)
	}
	held.line = nil
	if n := len(e.mem.Refunds()); n != 0 {
		t.Fatalf("%d refunds after the refusals", n)
	}
	got, err := e.acceptAs(t, "platform_admin", v.Request.ID, payments.AcceptInput{PaymentID: &po.payment.ID, TargetIBAN: ibanSender,
		RefundReason: payments.RefundReasonGoodwill, Note: "Alıcıyla görüşüldü"})
	if err != nil || got.Refund == nil || *got.Refund.TargetIBAN != ibanSender || got.Refund.Reason != payments.RefundReasonGoodwill ||
		got.Refund.ReasonNote == nil || got.Refund.RequestedBy != id("platform_admin") {
		t.Fatalf("accept with overrides = %+v, %v", got, err)
	}
	if ev := e.audit.Of(authz.PaymentsRefundRequestAccept); len(ev) != 1 || ev[0].Metadata["iban_changed"] != true {
		t.Fatalf("accept audit = %+v", ev)
	}
	// An order paid by two transfers: no payment holds the whole line; the
	// refund of the named one stays within it (finance splits by hand).
	p2 := e.order(t, orderSpec{})
	for _, part := range []int64{600_000, 600_000} {
		if _, err := e.match(t, e.transfer(t, acct, part, p2.order.OrderNo, true), p2, ""); err != nil {
			t.Fatal(err)
		}
	}
	big, err := e.askRefund(t, "company_admin", p2.order.ID, p2.lines[0].ID, 1_200_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.acceptAs(t, "finance_admin", big.Request.ID, payments.AcceptInput{}); problemtestStatus(t, err) != 422 {
		t.Fatalf("no payment holds the amount = %v", err)
	}
	pays, err := e.store.PaymentsOfOrder(t.Context(), p2.order.ID)
	if err != nil || len(pays) != 2 {
		t.Fatalf("payments = %+v, %v", pays, err)
	}
	if _, err := e.acceptAs(t, "finance_admin", big.Request.ID, payments.AcceptInput{PaymentID: &pays[0].ID}); !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("beyond the named payment = %v", err)
	}
}

// TestRefundRequest_AudienceViews: the buyer's members and staff read the
// IBAN masked and the requester; the seller's staff read the request
// without either; the lists are each org's own (the buyer's, the
// seller's), newest first, finance's queue oldest first; another company
// and a stranger get 404.
func TestRefundRequest_AudienceViews(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	first, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Second)
	second, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[1].ID, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]payments.RequestAudience{"company_member": payments.AudienceBuyer, "company_admin": payments.AudienceBuyer,
		"club_admin": payments.AudienceSeller, "support": payments.AudienceStaff, "finance_admin": payments.AudienceStaff} {
		a := actor(t, p)
		v, err := e.svc.GetRefundRequest(ctxOf(t, a), a, first.Request.ID)
		if err != nil || v.Audience != want {
			t.Errorf("%s read = %v, %v; want %s", p, v.Audience, err, want)
		}
	}
	for p, want := range map[string]error{"other_company_member": authz.ErrNotFound, "student": authz.ErrNotFound,
		"club_member": authz.ErrForbidden, "moderator": authz.ErrForbidden} {
		a := actor(t, p)
		if _, err := e.svc.GetRefundRequest(ctxOf(t, a), a, first.Request.ID); !errors.Is(err, want) {
			t.Errorf("%s read = %v, want %v", p, err, want)
		}
	}
	cla := actor(t, "club_admin")
	sales, err := e.svc.ListSalesRefundRequests(ctxOf(t, cla), cla, id("club_a"), payments.RefundRequestQuery{})
	if err != nil || len(sales.Items) != 1 || sales.Items[0].Request.ID != first.Request.ID || sales.Items[0].Audience != payments.AudienceSeller {
		t.Fatalf("club_a's sales requests (Koro's own line is not club_a's) = %+v, %v", sales, err)
	}
	cma := actor(t, "company_admin")
	mine, err := e.svc.ListOrgRefundRequests(ctxOf(t, cma), cma, id("company_a"), payments.RefundRequestQuery{Pending: true})
	if err != nil || len(mine.Items) != 2 || mine.Items[0].Request.ID != second.Request.ID {
		t.Fatalf("the buyer's list (newest first) = %+v, %v", mine, err)
	}
	fin := actor(t, "finance_admin")
	queue, err := e.svc.AdminListRefundRequests(ctxOf(t, fin), fin, payments.RefundRequestQuery{Limit: 1})
	if err != nil || len(queue.Items) != 1 || queue.Items[0].Request.ID != first.Request.ID || queue.Next == nil {
		t.Fatalf("the queue's first page (oldest first) = %+v, %v", queue, err)
	}
	next, err := e.svc.AdminListRefundRequests(ctxOf(t, fin), fin, payments.RefundRequestQuery{Limit: 1, After: queue.Next})
	if err != nil || len(next.Items) != 1 || next.Items[0].Request.ID != second.Request.ID {
		t.Fatalf("the queue's second page = %+v, %v", next, err)
	}
	if _, err := e.svc.AdminListRefundRequests(ctxOf(t, cma), cma, payments.RefundRequestQuery{}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("the buyer reads the queue = %v", err)
	}
	if _, err := e.svc.ListOrgRefundRequests(ctxOf(t, fin), fin, id("company_a"), payments.RefundRequestQuery{Status: "lost"}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("finance on the buyer's surface = %v", err)
	}
	if _, err := e.svc.ListOrgRefundRequests(ctxOf(t, cma), cma, id("company_a"), payments.RefundRequestQuery{Status: "lost"}); problemtestStatus(t, err) != 422 {
		t.Fatalf("unknown status = %v", err)
	}
	d, err := e.svc.AdminGetRefundRequest(ctxOf(t, fin), fin, first.Request.ID)
	if err != nil || d.LineRefundable != 1_200_000 || len(d.Payments) != 1 || d.Payments[0].PaymentID != po.payment.ID {
		t.Fatalf("admin detail = %+v, %v", d, err)
	}
}

// TestRefundRequest_Notices: after commit the finance admins and the
// seller's admins hear of a new request (the seller by e-mail too), the
// buyer's admins of its review, acceptance and rejection (e-mail for the
// decisions), the seller of the outcome, the reviewer of a withdrawal; a
// stale change sends nothing; a repeated task is a no-op.
func TestRefundRequest_Notices(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	e.queue.Reset()
	fin, cma := actor(t, "finance_admin"), actor(t, "company_admin")
	a, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.runRequestNotices(t)
	if got := e.sender.to("payments.refund_request_created", notify.ChannelInApp); !containsAll(got, id("finance_admin"), id("finance_admin_2")) || len(got) != 2 {
		t.Fatalf("finance told = %v", got)
	}
	if got := e.sender.to("payments.refund_request_received", notify.ChannelEmail); !slices.Equal(got, []uuid.UUID{id("club_admin")}) {
		t.Fatalf("seller e-mailed = %v", got)
	}
	if _, err := e.svc.ReviewRefundRequest(ctxOf(t, fin), fin, a.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.acceptAs(t, "finance_admin", a.Request.ID, payments.AcceptInput{}); err != nil {
		t.Fatal(err)
	}
	e.runRequestNotices(t) // in_review is stale now: only the acceptance is told
	if got := e.sender.to("payments.refund_request_in_review", notify.ChannelInApp); len(got) != 0 {
		t.Fatalf("stale review told = %v", got)
	}
	buyers := e.sender.to("payments.refund_request_accepted", notify.ChannelEmail)
	if !containsAll(buyers, id("company_admin"), id("company_owner_a")) || len(buyers) != 2 {
		t.Fatalf("buyers e-mailed of the acceptance = %v", buyers)
	}
	if got := e.sender.to("payments.refund_request_closed", notify.ChannelInApp); !slices.Equal(got, []uuid.UUID{id("club_admin")}) {
		t.Fatalf("seller told of the outcome = %v", got)
	}
	b, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[1].ID, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReviewRefundRequest(ctxOf(t, fin), fin, b.Request.ID); err != nil {
		t.Fatal(err)
	}
	e.runRequestNotices(t)
	if got := e.sender.to("payments.refund_request_in_review", notify.ChannelInApp); len(got) != 2 {
		t.Fatalf("buyers told of the review = %v", got)
	}
	if _, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, b.Request.ID); err != nil {
		t.Fatal(err)
	}
	e.runRequestNotices(t)
	if got := e.sender.to("payments.refund_request_withdrawn", notify.ChannelInApp); !slices.Equal(got, []uuid.UUID{id("finance_admin")}) {
		t.Fatalf("reviewer told of the withdrawal = %v", got)
	}
	for _, m := range e.sender.sent {
		if m.Template == "payments.refund_request_received" && m.Channel == notify.ChannelEmail {
			if m.Data["order_no"] != po.order.OrderNo || m.Data["iban"] != nil || m.Data["explanation"] != nil {
				t.Fatalf("seller e-mail data = %v", m.Data)
			}
		}
		if m.Channel == notify.ChannelInApp && m.LinkPath.IsZero() {
			t.Fatalf("%s without a link", m.Template)
		}
	}
}

// TestRefundRequest_KVKKExport: the requester's export lists the request
// with their explanation; the staff who decided it see their role without
// it; erasure retains both (financial records).
func TestRefundRequest_KVKKExport(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	v, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	if _, err := e.svc.RejectRefundRequest(ctxOf(t, fin), fin, v.Request.ID, "Kanıt yok"); err != nil {
		t.Fatal(err)
	}
	find := func(user uuid.UUID) map[string]any {
		sec, err := e.mod.Privacy.Export(t.Context(), user)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range sec.Items {
			if it["record"] == "refund_request" && it["refund_request_id"] == v.Request.ID.String() {
				return it
			}
		}
		return nil
	}
	if it := find(id("company_admin")); it == nil || it["explanation"] != explanation || !slices.Equal(it["roles"].([]string), []string{"requested"}) {
		t.Fatalf("requester's export = %+v", it)
	}
	if it := find(fin.UserID); it == nil || it["explanation"] != nil || !slices.Equal(it["roles"].([]string), []string{"decided"}) {
		t.Fatalf("decider's export = %+v", it)
	}
	if res, err := e.mod.Privacy.Erase(context.Background(), id("company_admin")); err != nil || res.Retained != 1 || res.RetainedReason == "" {
		t.Fatalf("erase = %+v, %v", res, err)
	}
}
