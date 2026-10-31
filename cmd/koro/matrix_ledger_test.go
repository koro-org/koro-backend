package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/platform/config"
)

// matrixFixtures posts fixtures/ledger.yaml (03 §7.3) through the ledger
// service over the in-memory store installed by useMemStore: payments
// first (a payment_received fills buyer_advances), then allocations.
//
// Under the matrix config ledger_seed: true (the BE-044 operations and
// scenarios set it) it also seeds the payout flow:
//
//	kind payable_line  a paid order of company_b (completed, bank transfer) with one fulfilled line of
//	                   payee (seller net 1,080,000, fulfilled 2026-10-02, hold 0) in orders' in-memory
//	                   store, and its payment and allocation in the ledger (the ledger owes the share)
//	kind receipt       an active payout_receipt upload of uploaded_by (files store), attached to payout
//	                   when the fixture names one
//	kind payout        a payout of line's payee with the line as its only item, the IBAN and holder of
//	                   the payee's approved account (parties fixtures) frozen on it, created_by;
//	                   approved (approved_by) and paid (receipt attached, payout_paid posted, the line
//	                   marked paid out) as status says
//
// The payout fixtures are seeded only under ledger_seed: an open payout of
// club_a, or club_a's unpaid-out lines, would block the orgs module's
// closeOrg checks (hooks.openCommerce).
func (w *ledgerWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx, at := context.Background(), time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	all := fx.Of("ledger")
	for _, template := range []string{"payment_received", "order_allocated"} {
		for _, f := range all {
			if f.Kind != "transaction" || f.Fields["template"] != template {
				continue
			}
			ref, err := fx.ID(f.Name)
			if err != nil {
				return err
			}
			amount, ok := f.Fields["amount"].(int)
			if !ok {
				return fmt.Errorf("fixture %s: amount must be an integer", f.Name)
			}
			var t ledger.Txn
			switch template {
			case "payment_received":
				t, err = ledger.PaymentReceived(ledger.ModelA, ref, int64(amount), at)
			case "order_allocated":
				payee := authztest.ID(f.Name + "_payee")
				t, err = ledger.OrderAllocated(ledger.ModelA, ref, []ledger.AllocationLine{{
					OrderLineID: authztest.ID(f.Name + "_line"), PayeeID: &payee, PayableAmount: int64(amount),
					SellerNetAmount: int64(amount) * 9 / 10, CommissionAmount: int64(amount) / 12,
					CommissionVATAmount: int64(amount) - int64(amount)*9/10 - int64(amount)/12,
				}}, at)
			}
			if err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
			if _, err := w.svc.Post(ctx, t); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		}
	}
	rank := map[string]int{"transaction": 0, "payable_line": 1, "receipt": 1, "payout": 2}
	for _, f := range all {
		if _, ok := rank[f.Kind]; !ok {
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	if seed, _ := matrixLedgerSeeds.Load(w); seed != true {
		return nil
	}
	if w.late == nil {
		return errors.New("ledger wiring was not built")
	}
	slices.SortStableFunc(all, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	for _, f := range all {
		var err error
		switch f.Kind {
		case "payable_line":
			err = w.matrixPayableLine(ctx, fx, f)
		case "receipt":
			err = w.matrixReceipt(ctx, fx, f)
		case "payout":
			err = w.matrixPayout(ctx, fx, f)
		}
		if err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	return nil
}

// matrixPayableLine seeds a paid order with one fulfilled line of the payee
// and posts its payment and allocation.
func (w *ledgerWiring) matrixPayableLine(ctx context.Context, fx *matrixFixtures, f fixture) error {
	mem, ok := w.late.orders.built.(*orders.MemStore)
	if !ok {
		return fmt.Errorf("the matrix needs orders' in-memory store, got %T", w.late.orders.built)
	}
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	payee, err := fx.Ref(f, "payee")
	if err != nil {
		return err
	}
	py, err := w.late.parties.payees.Get(ctx, payee)
	if err != nil {
		return err
	}
	buyer, party := authztest.ID("company_b"), authztest.ID("party_buyer_b")
	orderID := authztest.ID(f.Name + "_order")
	t, paid, fulfilled := matrixOrgTime, matrixOrgTime.Add(12*time.Hour), matrixOrgTime.Add(24*time.Hour)
	o := orders.Order{ID: orderID, OrderNo: matrixOrderNo(orderID), OrgID: buyer, OrgKind: "company",
		PlacedByUserID: authztest.ID("other_company_member"), BuyerPartyID: party,
		BuyerSnapshot: orders.BuyerSnapshot{Title: "Matris Alıcı A.Ş."}, ModelCode: orders.ModelA,
		CollectionMethod: orders.CollectBankTransfer, Status: orders.StatusCompleted, Currency: "TRY",
		SubtotalAmount: 1_000_000, VATAmount: 200_000, TotalAmount: 1_200_000, PayableAmount: 1_200_000,
		KoroCollectAmount: 1_200_000, PaidAmount: 1_200_000, ExpiresAt: t.Add(orders.DefaultPaymentWindow), PaidAt: &paid,
		PlacedAt: t}
	via := orders.ViaBuyerConfirmed
	l := orders.Line{ID: id, OrderID: orderID, OrgID: buyer, SellerOrgID: py.OrgID, PayeeID: &payee,
		OfferingID: authztest.ID(f.Name + "_offering"), ServiceID: authztest.ID("svc_booth"),
		CommercePolicyID: authztest.ID("policy_default"), TitleSnapshot: "Matris stant paketi", Quantity: 1,
		UnitPriceAmount: 1_000_000, NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000, GrossAmount: 1_200_000,
		CommissionRateBP: 1000, CommissionBasis: "net_excl_vat", CommissionPayer: "seller", CommissionAmount: 100_000,
		CommissionVATRateBP: 2000, CommissionVATAmount: 20_000, DisputeWindowDays: 7, SellerNetAmount: 1_080_000,
		PayableAmount: 1_200_000, FulfillmentStatus: orders.FulfillFulfilled, FulfilledAt: &fulfilled, FulfilledVia: &via}
	if err := mem.InsertOrder(ctx, o, []orders.Line{l}); err != nil {
		return err
	}
	pay, err := ledger.PaymentReceived(ledger.ModelA, authztest.ID(f.Name+"_payment"), 1_200_000, paid)
	if err != nil {
		return err
	}
	alloc, err := ledger.OrderAllocated(ledger.ModelA, orderID, []ledger.AllocationLine{{OrderLineID: id, PayeeID: &payee,
		PayableAmount: 1_200_000, SellerNetAmount: 1_080_000, CommissionAmount: 100_000, CommissionVATAmount: 20_000}}, paid)
	if err != nil {
		return err
	}
	for _, t := range []ledger.Txn{pay, alloc} {
		if _, err := w.svc.Post(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// matrixReceipt seeds an active payout_receipt upload (skipped by the
// no-op scanner, served while FILES_SCAN_REQUIRED=false; scan: infected
// seeds one the scan rejected after the upload, BE-154), attached to the
// fixture's payout when it names one.
func (w *ledgerWiring) matrixReceipt(ctx context.Context, fx *matrixFixtures, f fixture) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "uploaded_by")
	if err != nil {
		return err
	}
	var owner *uuid.UUID
	if _, ok := f.Fields["payout"]; ok {
		po, err := fx.Ref(f, "payout")
		if err != nil {
			return err
		}
		owner = &po
	}
	objects, err := w.late.files.memObjects()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	size := int64(len(matrixFilePDF))
	row := files.File{
		ID: id, Purpose: files.PurposePayoutReceipt, UploadedBy: &by, Bucket: testBuckets.Files,
		StorageKey: "matrix/" + f.Name, DeclaredMIME: files.MIMEPDF, SizeBytes: &size, SHA256: make([]byte, 32),
		Status: files.StatusActive, ScanStatus: files.ScanSkipped,
	}
	if scan, ok := f.Fields["scan"].(string); ok {
		row.ScanStatus = files.ScanStatus(scan)
	}
	if owner != nil {
		module := "ledger"
		row.AttachedModule, row.AttachedEntityID, row.AttachedAt = &module, owner, &now
	}
	objects.Upload(row.Bucket, row.StorageKey, matrixFilePDF, files.MIMEPDF)
	return w.late.files.store.Insert(ctx, row)
}

// matrixPayout seeds a payout of one line through its real states over the
// in-memory payout store (which applies the CHECKs of 000113).
func (w *ledgerWiring) matrixPayout(ctx context.Context, fx *matrixFixtures, f fixture) error {
	ps, ok := w.store.(ledger.PayoutStore)
	if !ok {
		return fmt.Errorf("the matrix needs the in-memory payout store, got %T", w.store)
	}
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	lineID, err := fx.Ref(f, "line")
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "created_by")
	if err != nil {
		return err
	}
	l, err := w.late.orders.orders.GetLine(ctx, lineID)
	if err != nil {
		return err
	}
	payee := *l.PayeeID
	py, err := w.late.parties.payees.Get(ctx, payee)
	if err != nil {
		return err
	}
	acct, err := w.late.parties.payees.ApprovedAccount(ctx, payee)
	if err != nil {
		// payee_uni_a waits for its first eye: its (IDOR) payout freezes the pending account.
		accts, aerr := w.late.parties.store.PayeeBankAccounts(ctx, payee)
		if aerr != nil || len(accts) == 0 {
			return errors.Join(err, aerr)
		}
		acct = accts[0]
	}
	t := matrixOrgTime.Add(36 * time.Hour)
	p := ledger.Payout{ID: id, PayeeID: payee, OrgID: py.OrgID, BankAccountID: acct.ID, IBANSnapshot: acct.IBAN,
		HolderSnapshot: acct.HolderName, Amount: l.SellerNetAmount, Currency: ledger.Currency,
		Status: ledger.StatusPendingApproval, CreatedBy: by}
	p, err = ps.InsertPayout(ctx, p, []ledger.PayoutItem{{PayoutID: id, OrderLineID: lineID, PayeeID: payee, Amount: p.Amount, CreatedAt: t}})
	if err != nil {
		return err
	}
	status, _ := f.Fields["status"].(string)
	if status == string(ledger.StatusPendingApproval) {
		return nil
	}
	approver, err := fx.Ref(f, "approved_by")
	if err != nil {
		return err
	}
	next := p
	approvedAt := t.Add(time.Hour)
	next.Status, next.ApprovedBy, next.ApprovedAt = ledger.StatusApproved, &approver, &approvedAt
	if p, err = ps.TransitionPayout(ctx, next, ledger.StatusPendingApproval); err != nil {
		return err
	}
	if status == string(ledger.StatusApproved) {
		return nil
	}
	if status != string(ledger.StatusPaid) {
		return fmt.Errorf("status %q: want pending_approval, approved or paid", status)
	}
	receipt, err := fx.Ref(f, "receipt") // seeded attached to this payout (receipt kind, payout field)
	if err != nil {
		return err
	}
	paidAt, ref := approvedAt.Add(time.Hour), "EFT-MATRIX-"+f.Name
	txn, err := ledger.PayoutPaid(ledger.ModelA, id, payee, p.Amount, paidAt)
	if err != nil {
		return err
	}
	if _, err := w.svc.Post(ctx, txn); err != nil {
		return err
	}
	if err := w.late.orders.orders.MarkLinesPaidOut(ctx, []uuid.UUID{lineID}, id); err != nil {
		return err
	}
	next = p
	next.Status, next.PaidBy, next.PaidAt, next.BankReference, next.ReceiptFileID = ledger.StatusPaid, &approver, &paidAt, &ref, &receipt
	_, err = ps.TransitionPayout(ctx, next, ledger.StatusApproved)
	return err
}

// matrixLedgerSeeds holds the ledger_seed config of each matrix app's
// wiring (the wiring struct is not the matrix's to extend).
var matrixLedgerSeeds sync.Map

// matrixConfig applies ledger_seed (bool): seed the payout fixtures.
func (w *ledgerWiring) matrixConfig(_ *config.Modules, key string, v any) (bool, error) {
	if key != "ledger_seed" {
		return false, nil
	}
	seed, ok := v.(bool)
	if !ok {
		return true, fmt.Errorf("ledger_seed %v: want true or false", v)
	}
	matrixLedgerSeeds.Store(w, seed)
	return true, nil
}
