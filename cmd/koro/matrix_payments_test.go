package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/civil"
)

// matrixValueDate is the value date of the transfer fixtures (the matrix
// clock is 2026-10-03T09:00:00Z).
var matrixValueDate = civil.Date{Year: 2026, Month: time.October, Day: 2}

// matrixFixtures seeds fixtures/payments.yaml (03 §7.3) into the in-memory
// stores installed by useMemStore:
//
//	kind account   a collection account of party_platform (status, iban, created_by; an active one
//	               approved by approved_by, default: the active default)
//	kind transfer  a transfer of amount on account recorded by entered_by (status unmatched, matched with
//	               matched_order / matched_by, return_requested by requested_by, return_approved by
//	               approved_by; order: the description carries that order's reference code)
//	kind file      an unattached, active upload of uploaded_by (purpose transfer_receipt or refund_receipt)
//	kind order_c   a model C order of org (party, placed_by) awaiting payment whose fee is collected, one
//	               line of club_a's payee_club_a; seeded into orders' store under orders_seed: true only
func (w *paymentsWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	mem, ok := w.built.(*payments.MemStore)
	if !ok {
		return fmt.Errorf("payments: the matrix needs the in-memory store, got %T", w.built)
	}
	if w.late == nil {
		return errors.New("payments wiring was not built")
	}
	rank := map[string]int{"file": 0, "account": 0, "transfer": 1, "order_c": 2}
	all := fx.Of("payments")
	slices.SortStableFunc(all, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	for _, f := range all {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		var ferr error
		switch f.Kind {
		case "file":
			ferr = w.matrixFile(ctx, fx, f, id)
		case "account":
			ferr = matrixAccount(ctx, fx, mem, f, id)
		case "transfer":
			ferr = matrixTransfer(ctx, fx, mem, f, id)
		case "order_c":
			ferr = w.matrixOrderC(ctx, fx, f, id)
		default:
			ferr = fmt.Errorf("unknown kind %q", f.Kind)
		}
		if ferr != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, ferr)
		}
	}
	return nil
}

func (w *paymentsWiring) matrixFile(ctx context.Context, fx *matrixFixtures, f fixture, id uuid.UUID) error {
	by, err := fx.Ref(f, "uploaded_by")
	if err != nil {
		return err
	}
	purpose, _ := f.Fields["purpose"].(string)
	size, t := int64(len(matrixFilePDF)), matrixOrgTime
	return w.late.files.store.Insert(ctx, files.File{
		ID: id, Purpose: files.Purpose(purpose), UploadedBy: &by, Bucket: testBuckets.Files, StorageKey: "matrix/" + f.Name,
		DeclaredMIME: files.MIMEPDF, SizeBytes: &size, SHA256: make([]byte, 32), Status: files.StatusActive,
		ScanStatus: files.ScanSkipped, CreatedAt: t, UpdatedAt: t,
	})
}

func matrixAccount(ctx context.Context, fx *matrixFixtures, mem *payments.MemStore, f fixture, id uuid.UUID) error {
	by, err := fx.Ref(f, "created_by")
	if err != nil {
		return err
	}
	party, err := fx.ID("party_platform")
	if err != nil {
		return err
	}
	number, _ := f.Fields["iban"].(string)
	status, _ := f.Fields["status"].(string)
	t := matrixOrgTime
	c, err := mem.InsertAccount(ctx, payments.CollectionAccount{ID: id, Label: "Matris " + f.Name, LegalPartyID: party, IBAN: number,
		HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank", Status: payments.AccountPending, CreatedBy: by, CreatedAt: t})
	if err != nil || payments.AccountStatus(status) == payments.AccountPending {
		return err
	}
	approver, err := fx.Ref(f, "approved_by")
	if err != nil {
		return err
	}
	def, _ := f.Fields["default"].(bool)
	c.Status, c.ApprovedBy, c.ApprovedAt, c.IsDefault = payments.AccountStatus(status), &approver, &t, def
	_, err = mem.UpdateAccount(ctx, c, payments.AccountPending)
	return err
}

func matrixTransfer(ctx context.Context, fx *matrixFixtures, mem *payments.MemStore, f fixture, id uuid.UUID) error {
	acct, err := fx.Ref(f, "account")
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "entered_by")
	if err != nil {
		return err
	}
	amount, _ := f.Fields["amount"].(int)
	t := matrixOrgTime
	tr := payments.IncomingTransfer{ID: id, CollectionAccountID: acct, ValueDate: matrixValueDate, Amount: int64(amount),
		Currency: payments.Currency, Description: "Matris " + f.Name, BankReference: "MATRIX-" + f.Name,
		Status: payments.TransferUnmatched, EntryMethod: payments.EntryManual, EnteredBy: by, CreatedAt: t}
	if v, _ := f.Fields["sender_iban"].(string); v != "" {
		tr.SenderIBAN = &v
	}
	if _, ok := f.Fields["order"]; ok {
		o, err := fx.Ref(f, "order")
		if err != nil {
			return err
		}
		no := matrixOrderNo(o)
		tr.Description, tr.ExtractedOrderNo = no+" STANT", &no
	}
	if tr, _, err = mem.InsertTransfer(ctx, tr); err != nil {
		return err
	}
	status, _ := f.Fields["status"].(string)
	step := func(next payments.IncomingTransfer) error {
		from := tr.Status
		tr, err = mem.UpdateTransfer(ctx, next, from)
		return err
	}
	switch payments.TransferStatus(status) {
	case "", payments.TransferUnmatched:
		return nil
	case payments.TransferMatched:
		o, err := fx.Ref(f, "matched_order")
		if err != nil {
			return err
		}
		m, err := fx.Ref(f, "matched_by")
		if err != nil {
			return err
		}
		next := tr
		next.Status, next.MatchedOrderID, next.MatchedBy, next.MatchedAt = payments.TransferMatched, &o, &m, &t
		return step(next)
	case payments.TransferReturnRequested, payments.TransferReturnApproved:
		r, err := fx.Ref(f, "requested_by")
		if err != nil {
			return err
		}
		why := "Matris iadesi"
		next := tr
		next.Status, next.ReturnRequestedBy, next.ReturnRequestedAt, next.ReturnReason, next.ReturnTargetIBAN =
			payments.TransferReturnRequested, &r, &t, &why, tr.SenderIBAN
		if err := step(next); err != nil || payments.TransferStatus(status) == payments.TransferReturnRequested {
			return err
		}
		a, err := fx.Ref(f, "approved_by")
		if err != nil {
			return err
		}
		next = tr
		next.Status, next.ReturnApprovedBy, next.ReturnApprovedAt = payments.TransferReturnApproved, &a, &t
		return step(next)
	}
	return fmt.Errorf("unknown transfer status %q", status)
}

// matrixOrderC seeds a model C order (the buyer pays club_a's payee directly,
// Koro's 10 % fee from the buyer is collected) into orders' store when the
// orders fixtures are seeded (orders_seed: open orders of company_a would
// block the orgs matrix's closeOrg otherwise).
func (w *paymentsWiring) matrixOrderC(ctx context.Context, fx *matrixFixtures, f fixture, id uuid.UUID) error {
	if seed, _ := matrixOrderSeeds.Load(&w.late.orders); seed != true {
		return nil
	}
	mem, ok := w.late.orders.built.(*orders.MemStore)
	if !ok {
		return fmt.Errorf("orders: the matrix needs the in-memory store, got %T", w.late.orders.built)
	}
	o, err := matrixOrder(fx, f, id)
	if err != nil {
		return err
	}
	o.Status, o.PaidAt, o.ModelCode, o.CollectionMethod = orders.StatusAwaitingPayment, nil, orders.ModelC, orders.CollectDirectToPayee
	ids := map[string]uuid.UUID{}
	for _, n := range []string{"club_a", "payee_club_a", "svc_booth", "policy_default"} {
		if ids[n], err = fx.ID(n); err != nil {
			return err
		}
	}
	pid, t := ids["payee_club_a"], matrixOrgTime
	l := orders.Line{ID: authztest.ID(f.Name + "_line"), OrderID: o.ID, OrgID: o.OrgID, SellerOrgID: ids["club_a"], PayeeID: &pid,
		OfferingID: authztest.ID(f.Name + "_offering"), ServiceID: ids["svc_booth"], CommercePolicyID: ids["policy_default"],
		TitleSnapshot: "Matris doğrudan ödeme", Quantity: 1,
		UnitPriceAmount: 1_000_000, NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000, GrossAmount: 1_200_000,
		CommissionRateBP: 1000, CommissionBasis: "net_excl_vat", CommissionPayer: "buyer", CommissionAmount: 100_000,
		CommissionVATRateBP: 2000, CommissionVATAmount: 20_000, DisputeWindowDays: 7, BuyerFeeAmount: 120_000,
		SellerNetAmount: 1_200_000, PayableAmount: 1_320_000, FulfillmentStatus: orders.FulfillPending, CreatedAt: t, UpdatedAt: t}
	o.SubtotalAmount, o.VATAmount, o.BuyerFeeAmount, o.TotalAmount, o.PayableAmount = 1_000_000, 200_000, 120_000, 1_320_000, 1_320_000
	o.KoroCollectAmount, o.DirectToPayeeAmount, o.PaidAmount = 120_000, 1_200_000, 120_000
	return mem.InsertOrder(ctx, o, []orders.Line{l})
}
