package payments_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/civil"
	"koro/internal/platform/idempotency"
)

// collectionLine marks a source line paid out through the orders port. The
// journal remains real: the completed refund creates the receivable.
type collectionLine struct {
	payments.Orders
	line uuid.UUID
}

func (o *collectionLine) GetLine(ctx context.Context, id uuid.UUID) (orders.Line, error) {
	l, err := o.Orders.GetLine(ctx, id)
	if err == nil && id == o.line {
		p := uuid.NewV7()
		l.PayoutID = &p
	}
	return l, err
}
func (e *env) collectionSource(t *testing.T, acct payments.CollectionAccount, amount int64) payments.IncomingTransfer {
	t.Helper()
	a := actor(t, "finance_admin")
	receipt := e.files.add(files.PurposeTransferReceipt)
	tr, err := e.svc.CreateTransfer(ctxOf(t, a), a, payments.TransferInput{CollectionAccountID: acct.ID, ValueDate: civil.From(e.clk.Now()), Amount: amount,
		Description: "Synthetic debt collection", BankReference: "COLLECT-" + uuid.NewV7().String(), ReceiptFileID: &receipt})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}
func collectionDebt(t *testing.T, e *env, w *collectionLine) (paidOrder, payments.CollectionAccount) {
	t.Helper()
	acct := e.activeAccount(t, ibanKoro, true)
	po := e.paid(t, acct, orderSpec{}, 0)
	w.line = po.lines[0].ID
	r := e.approved(t, lineRefund(po.payment.ID, w.line, 600_000))
	res, err := e.completeAs(t, "finance_admin", r.ID)
	if err != nil || res.Clawback != 540_000 {
		t.Fatalf("clawback=%+v %v", res, err)
	}
	return po, acct
}
func TestCollection_ClaimFourEyesReplayAndRelease(t *testing.T) {
	t.Parallel()
	w := &collectionLine{}
	e := newEnv(t, withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w }))
	_, acct := collectionDebt(t, e, w)
	a, b := actor(t, "finance_admin"), actor(t, "finance_admin_2")
	tr := e.collectionSource(t, acct, 540_000)
	in := payments.CollectionInput{IncomingTransferID: tr.ID, OrderLineID: w.line}
	ctx := idempotency.WithKey(ctxOf(t, a), idempotency.Key{Scope: "collection-create", Value: "create-1", Status: 201})
	c, err := e.svc.CreateCollection(ctx, a, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateCollection(ctx, a, in)
	if err != nil || again.ID != c.ID {
		t.Fatalf("replay=%+v %v", again, err)
	}
	if c.Amount != tr.Amount || c.BankReference != tr.BankReference || c.CollectionAccountID != acct.ID || c.ReceiptFileID != *tr.ReceiptFileID {
		t.Fatalf("derived receipt=%+v", c)
	}
	if _, err = e.svc.ApproveCollection(ctxOf(t, a), a, c.ID, good); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("self approval=%v", err)
	}
	if _, err = e.svc.ApproveCollection(ctxOf(t, b), b, c.ID, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("no reauth=%v", err)
	}
	extra := e.collectionSource(t, acct, 1)
	if _, err = e.svc.CreateCollection(ctxOf(t, a), a, payments.CollectionInput{IncomingTransferID: extra.ID, OrderLineID: w.line}); !errors.Is(err, payments.ErrCollectionExceedsReceivable) {
		t.Fatalf("reserved debt=%v", err)
	}
	if _, err = e.svc.RejectCollection(ctxOf(t, a), a, c.ID, "wrong source line"); err != nil {
		t.Fatal(err)
	}
	c, err = e.svc.CreateCollection(ctxOf(t, a), a, in)
	if err != nil {
		t.Fatalf("reuse released transfer: %v", err)
	}
	ctx = idempotency.WithKey(ctxOf(t, b), idempotency.Key{Scope: "collection-approve", Value: "approve-1", Status: 200})
	c, err = e.svc.ApproveCollection(ctx, b, c.ID, good)
	if err != nil || c.Status != payments.CollectionCollected {
		t.Fatalf("approve=%+v %v", c, err)
	}
	again, err = e.svc.ApproveCollection(ctx, b, c.ID, good)
	if err != nil || again.LedgerTransactionID == nil || *again.LedgerTransactionID != *c.LedgerTransactionID {
		t.Fatalf("approval replay=%+v %v", again, err)
	}
	if got, err := e.ledMod.LineReceivable(t.Context(), w.line); err != nil || got != 0 {
		t.Fatalf("receivable=%d %v", got, err)
	}
	if got := payeeBalance(t, e); got != 1_080_000 {
		t.Fatalf("collection changed seller payable: %d", got)
	}
	txns, err := e.ledStore.List(t.Context(), ledger.JournalFilter{Kind: ledger.KindAdjustment}, nil, 100)
	if err != nil || len(txns) != 1 {
		t.Fatalf("collection journal=%v %v", txns, err)
	}
	owner, entity := e.files.owner(c.ReceiptFileID)
	if owner != "payments" || entity != tr.ID {
		t.Fatalf("receipt ownership %s/%s", owner, entity)
	}
	if _, err = e.svc.CreateCollection(ctxOf(t, a), a, in); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("claimed transfer reuse=%v", err)
	}
	assertExportRole(t, e, b.UserID, "receivable_collection", "collection_id", c.ID, "approved")
	retained, err := e.mod.Privacy.Erase(t.Context(), b.UserID)
	if err != nil || retained.Retained < 1 || retained.RetainedReason == "" {
		t.Fatalf("collection retention=%+v %v", retained, err)
	}
}

func assertExportRole(t *testing.T, e *env, user uuid.UUID, record, idField string, entity uuid.UUID, role string) {
	t.Helper()
	section, err := e.mod.Privacy.Export(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range section.Items {
		roles, _ := item["roles"].([]string)
		if item["record"] == record && item[idField] == entity.String() && slices.Contains(roles, role) {
			return
		}
	}
	t.Fatalf("missing %s staff role %s in export: %+v", record, role, section.Items)
}

func TestRefundClose_ApprovedOnlyRetainsApprovalAndReleasesLimit(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "failed"}[failed], func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
			r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
			requester, fin := actor(t, "finance_admin_2"), actor(t, "finance_admin")
			if _, err := e.svc.CloseRefund(ctxOf(t, requester), requester, r.ID, failed, "Synthetic closure", good); !errors.Is(err, authz.ErrFourEyes) {
				t.Fatalf("requester closes=%v", err)
			}
			if _, err := e.svc.CloseRefund(ctxOf(t, fin), fin, r.ID, failed, "Synthetic closure", identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
				t.Fatalf("no proof=%v", err)
			}
			ctx := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{Scope: "refund-close", Value: "close-1", Status: 200})
			closed, err := e.svc.CloseRefund(ctx, fin, r.ID, failed, "Synthetic closure", good)
			if err != nil {
				t.Fatal(err)
			}
			if closed.ApprovedBy == nil || *closed.ApprovedBy != *r.ApprovedBy || closed.ApprovedAt == nil || !closed.ApprovedAt.Equal(*r.ApprovedAt) || closed.ClosedBy == nil || closed.CompletedAt != nil {
				t.Fatalf("approval evidence=%+v", closed)
			}
			if _, err = e.svc.CloseRefund(ctx, fin, r.ID, failed, "Synthetic closure", good); err != nil {
				t.Fatalf("replay=%v", err)
			}
			if len(e.refundTxns(t)) != 0 {
				t.Fatal("closing posted money")
			}
			assertExportRole(t, e, fin.UserID, "refund", "refund_id", closed.ID, "closed")
			replacement := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
			if _, err = e.completeAs(t, "finance_admin", replacement.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = e.svc.CloseRefund(ctxOf(t, fin), fin, replacement.ID, failed, "After payment", good); !errors.Is(err, payments.ErrInvalidState) {
				t.Fatalf("completed closes=%v", err)
			}
		})
	}
}
func TestRefundRequested_NoticeManualAutomaticStaleAndDedupe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 1_200_001)
	r, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, po.lines[0].ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, po.lines[0].ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.approve(t, "finance_admin_2", stale.ID); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, task := range e.queue.Tasks() {
		if task.Type != payments.NotifyRefundRequestedTaskName {
			continue
		}
		n++
		for _, h := range e.mod.Tasks() {
			if h.Name == task.Type {
				for range 2 {
					if err = h.Handle(t.Context(), task.Payload); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	if n != 3 {
		t.Fatalf("manual+auto+stale tasks=%d", n)
	}
	got := e.sender.to("payments.refund_requested", notify.ChannelInApp)
	if len(got) != 2 || (got[0] != id("finance_admin_2") || got[1] != id("finance_admin_2")) {
		t.Fatalf("notice recipients=%v (manual %s)", got, r.ID)
	}
}
