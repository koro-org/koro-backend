//go:build integration

package orders_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orders"
	"koro/internal/platform/domainevent"
)

// T-309: delivery starts while a refund still owns the order/line locks. Its
// earlier read sees an unrefunded line, so rejecting after the commit proves
// the decision was rechecked under the production row locks.
func TestRefundFollowups_DeliveryRechecksLockedRefund_Postgres(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"mark", "confirm", "admin", "auto"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			p := newPGEnv(t)
			first, second := p.w.offering(t, p.seller), p.w.offering(t, p.seller)
			bctx := p.buyerActor(t)
			buyer, err := authz.ActorFrom(bctx)
			if err != nil {
				t.Fatal(err)
			}
			views, err := p.create(bctx, line(first.id, 1), line(second.id, 1))
			if err != nil {
				t.Fatal(err)
			}
			o := views[0].Order
			target := views[0].Lines[0]
			if _, err := p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), o.PayableAmount); err != nil {
				t.Fatal(err)
			}
			seller, sctx := p.sellerActor(t)
			if action != "mark" {
				if _, err := p.mod.Service.MarkLineFulfilled(sctx, seller, target.ID, orders.MarkInput{}); err != nil {
					t.Fatal(err)
				}
			}
			if action == "admin" {
				if _, err := p.mod.Service.DisputeLine(bctx, buyer, target.ID, "Synthetic missing delivery"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := p.mod.Orders.GetLine(t.Context(), target.ID)
			if err != nil {
				t.Fatal(err)
			}
			admin := actor(t, "finance_admin")
			admin.UserID = pgUser(t, p.d)
			p.clk.Advance(8 * 24 * time.Hour)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			locked, resume := make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(resume) })
			refundDone := make(chan error, 1)
			go func() {
				refundDone <- p.d.Tx.WithTx(ctx, func(txctx context.Context) error {
					if err := p.mod.Orders.ApplyRefund(txctx, o.ID, &target.ID, target.PayableAmount); err != nil {
						return err
					}
					close(locked)
					select {
					case <-resume:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-locked:
			case err := <-refundDone:
				t.Fatalf("refund before gate: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			deliveryDone := make(chan error, 1)
			go func() {
				var err error
				switch action {
				case "mark":
					_, err = p.mod.Service.MarkLineFulfilled(authz.WithActor(ctx, seller), seller, target.ID, orders.MarkInput{})
				case "confirm":
					_, err = p.mod.Service.ConfirmLine(authz.WithActor(ctx, buyer), buyer, target.ID)
				case "admin":
					_, err = p.mod.Service.AdminResolveDispute(authz.WithActor(ctx, admin), admin, target.ID, orders.ResolveInput{Outcome: orders.OutcomeFulfilled, Note: "Synthetic review"})
				case "auto":
					for _, job := range p.mod.Jobs() {
						if job.Name == orders.CloseWindowsJobName {
							err = job.Handle(ctx)
							break
						}
					}
				}
				deliveryDone <- err
			}()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				var waiting bool
				if err := p.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
     AND pid <> pg_backend_pid() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-deliveryDone:
					t.Fatalf("delivery did not wait for refund: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-tick.C:
				}
			}
			release.Do(func() { close(resume) })
			if err := <-refundDone; err != nil {
				t.Fatal(err)
			}
			err = <-deliveryDone
			if action == "auto" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				wantProblem(t, err, 409, "orders.invalid_state")
			}
			after, err := p.mod.Orders.GetLine(t.Context(), target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.FulfillmentStatus != before.FulfillmentStatus || after.FulfilledAt != nil || after.RefundedAmount != target.PayableAmount {
				t.Fatalf("refunded line delivered: %+v", after)
			}
			order, _, err := p.mod.Orders.ForBilling(t.Context(), o.ID)
			if err != nil || order.Status != orders.StatusPartiallyRefunded {
				t.Fatalf("other line's paid order lost: %+v %v", order, err)
			}
			for _, ev := range p.outbox.Events() {
				if ev.Topic == domainevent.TopicOrderLineFulfilled {
					t.Fatal("refunded line emitted fulfilled event")
				}
			}
		})
	}
}

func TestRefundFollowups_ZeroFeeModelCAutoDelivery_Postgres(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t)
	off := p.w.offering(t, p.seller)
	pgExec(t, p.d, `UPDATE commerce_policies SET valid_to=$2 WHERE id=$1`, off.policy, policyFrom.Add(time.Hour))
	pgExec(t, p.d, `INSERT INTO commerce_policies (id,scope,scope_id,model_code,collection_method,commission_rate_bp,commission_basis,commission_payer,valid_from,created_by)
 VALUES ($1,'offering',$2,'C','direct_to_payee',0,'net_excl_vat','buyer',$3,$4)`, uuid.NewV7(), off.id, policyFrom.Add(time.Hour), p.buyer.user)
	views, err := p.create(p.buyerActor(t), line(off.id, 1))
	if err != nil {
		t.Fatal(err)
	}
	o, l := views[0].Order, views[0].Lines[0]
	if o.KoroCollectAmount != 0 || o.DirectToPayeeAmount <= 0 {
		t.Fatalf("not a zero-fee C fixture: %+v", o)
	}
	if eff, err := p.mod.Orders.ConfirmDirectPayment(t.Context(), o.ID, uuid.NewV7()); err != nil || !eff.Paid {
		t.Fatalf("direct confirmation: %+v %v", eff, err)
	}
	seller, ctx := p.sellerActor(t)
	if _, err := p.mod.Service.MarkLineFulfilled(ctx, seller, l.ID, orders.MarkInput{}); err != nil {
		t.Fatal(err)
	}
	p.clk.Advance(8 * 24 * time.Hour)
	for _, job := range p.mod.Jobs() {
		if job.Name == orders.CloseWindowsJobName {
			if err := job.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	stored, err := p.mod.Orders.GetLine(t.Context(), l.ID)
	if err != nil || stored.FulfillmentStatus != orders.FulfillFulfilled || stored.FulfilledVia == nil || *stored.FulfilledVia != orders.ViaWindowElapsed {
		t.Fatalf("zero-fee C delivery: %+v %v", stored, err)
	}
	if payable, err := p.mod.Orders.PayableLines(t.Context(), p.seller.payee, p.clk.Now().AddDate(0, 0, 30)); err != nil || len(payable) != 0 {
		t.Fatalf("direct line became Koro payable: %+v %v", payable, err)
	}
}
