package main

import (
	"context"
	"fmt"
	"time"

	"koro/internal/authz/authztest"
	"koro/internal/ledger"
)

// matrixFixtures posts fixtures/ledger.yaml (03 §7.3) through the ledger
// service over the in-memory store installed by useMemStore: payments
// first (a payment_received fills buyer_advances), then allocations.
func (w *ledgerWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx, at := context.Background(), time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, template := range []string{"payment_received", "order_allocated"} {
		for _, f := range fx.Of("ledger") {
			if f.Kind != "transaction" {
				return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
			}
			if f.Fields["template"] != template {
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
	return nil
}
