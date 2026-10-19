package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/queue"
)

// Task names of the plan consumers and the invoice notices (01 §6.2;
// "<module>.<verb>_<object>").
const (
	PlanOnPaymentTaskName     = "billing.plan_on_payment"     // payment.captured → on_payment documents
	PlanOnFulfillmentTaskName = "billing.plan_on_fulfillment" // order_line.fulfilled → on_fulfillment documents
	PlanOnPayoutTaskName      = "billing.plan_on_payout"      // payout.paid → Koro's commission invoice
	PlanOnRefundTaskName      = "billing.plan_on_refund"      // payment.refunded → cancel the plan / credit note
	SendInvoiceTaskName       = "billing.send_invoice"        // invoice.issued → the PDF to the recipient
	NotifyInvoiceTaskName     = "billing.notify_invoice"      // A: planned, uploaded, mismatch notices
)

// maxDescriptionRunes is invoice_lines_description_chk.
const maxDescriptionRunes = 300

// expected is one document the plan expects, not stored yet.
type expected struct {
	inv   Invoice
	lines []Line
}

// planResource is the authz view of the plan (system action, 03 §3.7).
var planResource = authz.Resource{Type: authz.TypeInvoice, Attrs: map[string]string{attrRecipientOrg: ""}}

// plan stores the documents build expects in one transaction: a document
// whose key exists is skipped (a repeated event), a new one is audited as
// billing.invoice.plan and its uploaders are told after the commit.
func (s *service) plan(ctx context.Context, job string, build func(ctx context.Context) ([]expected, error)) error {
	sys := authz.SystemActor(job)
	if err := authz.Can(ctx, sys, authz.BillingInvoicePlan, planResource); err != nil {
		return queue.Permanent(err)
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		docs, err := build(ctx)
		if err != nil {
			return err
		}
		for _, x := range docs {
			stored, created, err := s.d.Store.InsertInvoice(ctx, x.inv, x.lines)
			if err != nil {
				return err
			}
			if !created {
				continue
			}
			if err := s.d.Audit.Log(ctx, invoiceEvent(sys, authz.BillingInvoicePlan, stored, "",
				map[string]any{"trigger": string(stored.Trigger), "lines": len(x.lines)})); err != nil {
				return err
			}
			s.notice(ctx, stored, noticePlanned)
			s.d.Log.LogAttrs(ctx, slog.LevelInfo, "invoice planned", slog.String("invoice_id", stored.ID.String()),
				slog.String("kind", string(stored.Kind)), slog.String("trigger", string(stored.Trigger)))
		}
		return nil
	})
}

// ---- event handlers

// handlePaymentCaptured plans the on_payment documents of an order once
// the payment completed it (a partial payment plans nothing yet).
func (s *service) handlePaymentCaptured(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PaymentCaptured](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if !p.OrderPaid {
		return nil
	}
	return s.plan(ctx, PlanOnPaymentTaskName, func(ctx context.Context) ([]expected, error) {
		o, lines, ok, err := s.billingOrder(ctx, p.OrderID)
		if err != nil || !ok {
			return nil, err
		}
		return s.onPayment(ctx, o, lines)
	})
}

// handleLineFulfilled plans the on_fulfillment service invoice of a line.
func (s *service) handleLineFulfilled(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrderLineFulfilled](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.plan(ctx, PlanOnFulfillmentTaskName, func(ctx context.Context) ([]expected, error) {
		o, lines, ok, err := s.billingOrder(ctx, p.OrderID)
		if err != nil || !ok {
			return nil, err
		}
		for _, l := range lines {
			if l.ID == p.OrderLineID {
				return s.onFulfilled(ctx, o, l)
			}
		}
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice plan: fulfilled line not in its order",
			slog.String("order_id", p.OrderID.String()), slog.String("order_line_id", p.OrderLineID.String()))
		return nil, nil
	})
}

// handlePayoutPaid plans Koro's commission invoice to the payee of a paid
// payout.
func (s *service) handlePayoutPaid(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PayoutPaid](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.plan(ctx, PlanOnPayoutTaskName, func(ctx context.Context) ([]expected, error) {
		return s.onPayout(ctx, p.PayoutID, p.PayeeID)
	})
}

// billingOrder reads an order with its lines; a missing order (a stale
// event) is skipped.
func (s *service) billingOrder(ctx context.Context, orderID uuid.UUID) (orders.Order, []orders.Line, bool, error) {
	o, lines, err := s.d.Orders.ForBilling(ctx, orderID)
	switch {
	case errors.Is(err, orders.ErrNotFound):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice plan: order not found", slog.String("order_id", orderID.String()))
		return orders.Order{}, nil, false, nil
	case err != nil:
		return orders.Order{}, nil, false, err
	}
	return o, lines, true, nil
}

// modelPlanned reports whether billing plans the documents of the order's
// model now (T-11): A fully, C its fee only; A2 and B are not decided.
func (s *service) modelPlanned(ctx context.Context, o orders.Order) bool {
	switch o.ModelCode {
	case orders.ModelA, orders.ModelC:
		return true
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice plan skipped: the invoice direction of the model is not decided (T-11)",
		slog.String("order_id", o.ID.String()), slog.String("model", o.ModelCode))
	return false
}

// onPayment returns the documents a completed payment expects: model A —
// the on_payment service invoices of its lines and Koro's platform fee
// invoice when the buyer pays the commission; model C — Koro's fee only.
func (s *service) onPayment(ctx context.Context, o orders.Order, lines []orders.Line) ([]expected, error) {
	if !o.Status.PaidState() || !s.modelPlanned(ctx, o) {
		return nil, nil
	}
	var out []expected
	if o.ModelCode == orders.ModelA {
		for _, l := range lines {
			if l.FulfillmentStatus == orders.FulfillCancelled {
				continue
			}
			terms, err := s.terms(ctx, l)
			if err != nil {
				return nil, err
			}
			if terms.Trigger != catalog.InvoiceOnPayment {
				continue
			}
			x, err := s.serviceSale(ctx, o, l, terms, TriggerOnPayment)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
	}
	fee, ok, err := s.platformFee(ctx, o, lines)
	if err != nil {
		return nil, err
	}
	if ok {
		out = append(out, fee)
	}
	return out, nil
}

// onFulfilled returns the service invoice of a fulfilled model A line whose
// offering invoices on fulfilment.
func (s *service) onFulfilled(ctx context.Context, o orders.Order, l orders.Line) ([]expected, error) {
	if l.FulfillmentStatus != orders.FulfillFulfilled || !s.modelPlanned(ctx, o) || o.ModelCode != orders.ModelA {
		return nil, nil
	}
	terms, err := s.terms(ctx, l)
	if err != nil {
		return nil, err
	}
	if terms.Trigger != catalog.InvoiceOnFulfillment {
		return nil, nil
	}
	x, err := s.serviceSale(ctx, o, l, terms, TriggerOnFulfillment)
	if err != nil {
		return nil, err
	}
	return []expected{x}, nil
}

// terms reads the invoice terms of a line (catalog); a provider automation
// is still planned for upload until BE-053 (logged).
func (s *service) terms(ctx context.Context, l orders.Line) (catalog.InvoiceTerms, error) {
	t, err := s.d.Offerings.InvoiceTerms(ctx, l.OfferingID, l.CommercePolicyID)
	if err != nil {
		return catalog.InvoiceTerms{}, fmt.Errorf("billing: invoice terms of line %s: %w", l.ID, err)
	}
	if t.Automation == catalog.InvoiceProvider && s.d.Provider == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice automation 'provider' is not wired: the invoice is planned for upload",
			slog.String("order_line_id", l.ID.String()))
	}
	return t, nil
}

// serviceSale is the service invoice of one line to the buyer: the payee's
// (seller_uploaded) or, for a platform-native line, Koro's (manual_upload).
func (s *service) serviceSale(ctx context.Context, o orders.Order, l orders.Line, t catalog.InvoiceTerms, trig Trigger) (expected, error) {
	var (
		issuer    parties.LegalParty
		issuerOrg *uuid.UUID
		mode      = ModeManualUpload
		err       error
	)
	if l.PayeeID != nil {
		py, err := s.d.Payees.Get(ctx, *l.PayeeID)
		if err != nil {
			return expected{}, fmt.Errorf("billing: payee of line %s: %w", l.ID, err)
		}
		if issuer, err = s.d.Parties.Get(ctx, py.LegalPartyID); err != nil {
			return expected{}, fmt.Errorf("billing: party of payee %s: %w", py.ID, err)
		}
		org := py.OrgID
		issuerOrg, mode = &org, ModeSellerUploaded
	} else if issuer, err = s.platformParty(ctx); err != nil {
		return expected{}, err
	}
	desc := l.TitleSnapshot
	if t.Description != nil && *t.Description != "" {
		desc = *t.Description + " – " + l.TitleSnapshot
	}
	ln := Line{
		OrderLineID: &l.ID, Description: desc, Quantity: l.Quantity, UnitPriceAmount: l.UnitPriceAmount,
		NetAmount: l.NetAmount, VATRateBP: l.VATRateBP, VATAmount: l.VATAmount, VATExemptionCode: t.VATExemptionCode,
		VATWithholdingCode: t.VATWithholdingCode, VATWithheldAmount: l.VATWithheldAmount,
	}
	buyerOrg := o.OrgID
	return s.document(Invoice{
		OrderID: &o.ID, Kind: KindServiceSale, IssuerPartyID: issuer.ID, RecipientPartyID: o.BuyerPartyID,
		IssuerOrgID: issuerOrg, RecipientOrgID: &buyerOrg, IssuerSnapshot: partySnapshot(issuer),
		RecipientSnapshot: buyerSnapshot(o.BuyerSnapshot), IssuingMode: mode, Trigger: trig,
		IdempotencyKey: key(KindServiceSale, o.ID, l.ID.String()),
	}, []Line{ln})
}

// platformFee is Koro's fee invoice to the buyer for the lines whose buyer
// pays the commission (model A, payer buyer; model C's fee).
func (s *service) platformFee(ctx context.Context, o orders.Order, lines []orders.Line) (expected, bool, error) {
	var ls []Line
	for _, l := range lines {
		if l.CommissionPayer != "buyer" || l.CommissionAmount <= 0 || l.FulfillmentStatus == orders.FulfillCancelled {
			continue
		}
		id := l.ID
		ls = append(ls, Line{OrderLineID: &id, Description: "Platform hizmet bedeli – " + l.TitleSnapshot, Quantity: 1,
			UnitPriceAmount: l.CommissionAmount, NetAmount: l.CommissionAmount, VATRateBP: l.CommissionVATRateBP,
			VATAmount: l.CommissionVATAmount})
	}
	if len(ls) == 0 {
		return expected{}, false, nil
	}
	koro, err := s.platformParty(ctx)
	if err != nil {
		return expected{}, false, err
	}
	buyerOrg := o.OrgID
	x, err := s.document(Invoice{
		OrderID: &o.ID, Kind: KindPlatformFee, IssuerPartyID: koro.ID, RecipientPartyID: o.BuyerPartyID,
		RecipientOrgID: &buyerOrg, IssuerSnapshot: partySnapshot(koro), RecipientSnapshot: buyerSnapshot(o.BuyerSnapshot),
		IssuingMode: ModeManualUpload, Trigger: TriggerOnPayment, IdempotencyKey: key(KindPlatformFee, o.ID, "all"),
	}, ls)
	return x, err == nil, err
}

// onPayout returns Koro's commission invoice to the payee for the paid-out
// model A lines whose seller pays the commission.
func (s *service) onPayout(ctx context.Context, payoutID, payeeID uuid.UUID) ([]expected, error) {
	lines, err := s.d.Orders.PayoutLines(ctx, payoutID)
	if err != nil {
		return nil, err
	}
	byOrder := map[uuid.UUID]orders.Order{}
	var ls []Line
	for _, l := range lines {
		if l.CommissionPayer != "seller" || l.CommissionAmount <= 0 {
			continue
		}
		o, ok := byOrder[l.OrderID]
		if !ok {
			if o, _, err = s.d.Orders.ForBilling(ctx, l.OrderID); err != nil {
				return nil, fmt.Errorf("billing: order of payout line %s: %w", l.ID, err)
			}
			byOrder[l.OrderID] = o
		}
		if o.ModelCode != orders.ModelA {
			s.modelPlanned(ctx, o)
			continue
		}
		id := l.ID
		ls = append(ls, Line{OrderLineID: &id, Description: "Komisyon – " + o.OrderNo + " " + l.TitleSnapshot, Quantity: 1,
			UnitPriceAmount: l.CommissionAmount, NetAmount: l.CommissionAmount, VATRateBP: l.CommissionVATRateBP,
			VATAmount: l.CommissionVATAmount})
	}
	if len(ls) == 0 {
		return nil, nil
	}
	py, err := s.d.Payees.Get(ctx, payeeID)
	if err != nil {
		return nil, fmt.Errorf("billing: payee of payout %s: %w", payoutID, err)
	}
	party, err := s.d.Parties.Get(ctx, py.LegalPartyID)
	if err != nil {
		return nil, fmt.Errorf("billing: party of payee %s: %w", py.ID, err)
	}
	koro, err := s.platformParty(ctx)
	if err != nil {
		return nil, err
	}
	payout, org := payoutID, py.OrgID
	x, err := s.document(Invoice{
		PayoutID: &payout, Kind: KindCommission, IssuerPartyID: koro.ID, RecipientPartyID: party.ID, RecipientOrgID: &org,
		IssuerSnapshot: partySnapshot(koro), RecipientSnapshot: partySnapshot(party), IssuingMode: ModeManualUpload,
		Trigger: TriggerOnPayout, IdempotencyKey: key(KindCommission, payoutID, "all"),
	}, ls)
	if err != nil {
		return nil, err
	}
	return []expected{x}, nil
}

// platformParty returns Koro's legal party; without it (bootstrap-commerce
// not run) Koro's invoices cannot be planned: the task fails and retries.
func (s *service) platformParty(ctx context.Context) (parties.LegalParty, error) {
	p, err := s.d.Parties.Platform(ctx)
	if errors.Is(err, parties.ErrNotFound) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "invoice plan: Koro's legal party is missing (koro seed bootstrap-commerce)",
			slog.Bool("alarm", true))
	}
	if err != nil {
		return parties.LegalParty{}, fmt.Errorf("billing: Koro's legal party: %w", err)
	}
	return p, nil
}

// document completes a planned invoice: ID, state, line positions and
// totals (platform/money, 06 H-6).
func (s *service) document(inv Invoice, lines []Line) (expected, error) {
	inv.ID, inv.Status, inv.DocumentType, inv.Provider, inv.Currency = uuid.NewV7(), StatusPlanned, DocUnknown, ProviderNone, Currency
	var net, vat, withheld []int64
	for i := range lines {
		l := &lines[i]
		l.ID, l.InvoiceID, l.Position = uuid.NewV7(), inv.ID, int16(i+1)
		if utf8.RuneCountInString(l.Description) > maxDescriptionRunes {
			l.Description = string([]rune(l.Description)[:maxDescriptionRunes])
		}
		total, err := money.Add(l.NetAmount, l.VATAmount)
		if err != nil {
			return expected{}, fmt.Errorf("billing: invoice line total: %w", err)
		}
		l.TotalAmount = total
		net, vat, withheld = append(net, l.NetAmount), append(vat, l.VATAmount), append(withheld, l.VATWithheldAmount)
	}
	var err error
	if inv.NetAmount, err = money.Sum(net...); err == nil {
		if inv.VATAmount, err = money.Sum(vat...); err == nil {
			if inv.VATWithheldAmount, err = money.Sum(withheld...); err == nil {
				if inv.TotalAmount, err = money.Add(inv.NetAmount, inv.VATAmount); err == nil {
					inv.PayableAmount, err = money.Sub(inv.TotalAmount, inv.VATWithheldAmount)
				}
			}
		}
	}
	if err != nil {
		return expected{}, fmt.Errorf("billing: invoice totals: %w", err)
	}
	return expected{inv: inv, lines: lines}, nil
}

// key is the idempotency key of a planned document (02 §22.1):
// "<kind>:<order_id|payout_id>:<order_line_id|all|invoice_id>:<seq>".
func key(k Kind, source uuid.UUID, part string) string {
	return string(k) + ":" + source.String() + ":" + part + ":1"
}

func partySnapshot(p parties.LegalParty) Snapshot {
	out := Snapshot{Title: p.Title, Address: SnapshotAddress(p.Address)}
	if p.TaxOffice != nil {
		out.TaxOffice = *p.TaxOffice
	}
	if p.TaxNo != nil {
		out.TaxNo = *p.TaxNo
	}
	if p.Email != nil {
		out.Email = *p.Email
	}
	return out
}

func buyerSnapshot(b orders.BuyerSnapshot) Snapshot {
	return Snapshot{Title: b.Title, TaxOffice: b.TaxOffice, TaxNo: b.TaxNo, Address: SnapshotAddress(b.Address), Email: b.Email}
}

// invoiceEvent is the audit record of an invoice write (IDs, kind, state,
// amounts; never the parties' data).
func invoiceEvent(a authz.Actor, act authz.Action, inv Invoice, reason string, meta map[string]any) audit.Event {
	id := inv.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["kind"] = string(inv.Kind)
	meta["status"] = string(inv.Status)
	meta["total_amount"] = inv.TotalAmount
	if inv.OrderID != nil {
		meta["order_id"] = inv.OrderID.String()
	}
	if inv.PayoutID != nil {
		meta["payout_id"] = inv.PayoutID.String()
	}
	org := inv.IssuerOrgID
	if org == nil {
		org = inv.RecipientOrgID
	}
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: org, ResourceType: authz.TypeInvoice, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}
