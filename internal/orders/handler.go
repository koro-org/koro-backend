package orders

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the orders operations (04 §2.15) to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "orders", huma.Operation{
		OperationID: "createOrder", Method: http.MethodPost, Path: "/orgs/{org_id}/orders",
		Summary: "Place an order", DefaultStatus: http.StatusCreated,
		Description: "A company admin buys offerings for the company, billed to a verified buyer party of the company " +
			"(422 orders.buyer_party_required). The lines are the cart: 1-20 offerings of any sellers, one line each. " +
			"Prices, VAT, withholding and commission are computed per line on the server from the offering and the " +
			"commerce policy in effect (none: 409 catalog.no_commerce_policy); lines under different money models become " +
			"separate orders, each with its own reference code, while the lines of several sellers under one model share " +
			"one order and one transfer. The stock of every line is reserved in the same transaction: when a line is " +
			"refused (sold out, not on sale, the per-buyer limit, no policy, an inactive payee) nothing is placed and the " +
			"problem names every refused line of the cart, one errors[] entry each (location body.lines[i].offering_id, " +
			"message the refusal's title, code the refusal's own code from problems.json; BE-139, BE-140): first the lines that cannot be priced, in request order, then the " +
			"stock refusals in offering ID order. status and code are the first refusal's, which is also errors[0] " +
			"(errors[0].code is the problem's code). " +
			"An unpaid order expires after the " +
			"payment window and releases its stock. When an offering of an unpaid order is cancelled before any money " +
			"arrived, only its line leaves the order (lines[].dropped_at): the reference code stays, the amounts become " +
			"the sums of the other lines and the buyer is told; the last line, or money already paid, cancels the order. " +
			"quoteOrder previews the same body without writing anything (BE-136).",
	}, op.Meta{Action: string(authz.OrdersOrderCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyRequired}, h.create)

	op.Register(api, "orders", huma.Operation{
		OperationID: "quoteOrder", Method: http.MethodPost, Path: "/orgs/{org_id}/orders/quote",
		Summary: "Preview the orders of a cart",
		Description: "Read-only (BE-136): the orders createOrder would place now for the same body, computed by " +
			"createOrder's own code — the same checks, the same per-line prices, VAT, withholding and buyer fee, the " +
			"same split into one order per money model and the same totals (payable_amount, koro_collect_amount, " +
			"direct_to_payee_amount) — and written nowhere: no stock is reserved, no order or reference code, and no " +
			"Idempotency-Key (it writes nothing to replay). Each line carries what the cart shows of its offering " +
			"(title, seller, deliverables, unit price excluding VAT, VAT rate), so the cart needs no getOffering per " +
			"line. A refused cart is refused as createOrder refuses it: the same status and code, and the same errors[] " +
			"naming every refused line (body.lines[i].offering_id). The stock is only read: another buyer may still take the " +
			"last units, or a policy change the prices, before createOrder, whose answer is final. The buyer never " +
			"sees the commission.",
	}, op.Meta{Action: string(authz.OrdersOrderQuote), Surface: op.SurfaceOrg, RateLimit: op.RateSearch,
		Idempotency: op.IdempotencyOptional}, h.quote)

	op.Register(api, "orders", huma.Operation{
		OperationID: "listOrgOrders", Method: http.MethodGet, Path: "/orgs/{org_id}/orders",
		Summary:     "List the orders of a company",
		Description: "Newest placement first.",
	}, op.Meta{Action: string(authz.OrdersOrderList), Surface: op.SurfaceOrg}, h.listOrg)

	op.Register(api, "orders", huma.Operation{
		OperationID: "getOrder", Method: http.MethodGet, Path: "/orders/{order_id}",
		Summary: "Get an order",
		Description: "The buyer's members and platform staff get the whole order (view buyer: totals, payment, every " +
			"line; never the commission, never a payee bank account: the direct transfers of an unpaid model C order " +
			"are in getOrderPaymentInstructions). MFA-verified buyer administrators get direct_payments_status; when a " +
			"payee has no approved bank account (unavailable) the order still reads in full and only the payment " +
			"instructions answer 409. A seller's club admin or university representative gets only its " +
			"own lines with its net share and the commission, the buyer's title, and status = seller_status: the " +
			"status of its own lines only, never another seller's dispute or refund (view seller; Q-31). Anyone else: 404. " +
			"A line with dropped_at left the unpaid order when its offering was cancelled: the order's amounts are the " +
			"sums of the other lines.",
	}, op.Meta{Action: string(authz.OrdersOrderRead), Surface: op.SurfaceResource}, h.get)

	op.Register(api, "orders", huma.Operation{
		OperationID: "getOrderPaymentInstructions", Method: http.MethodGet, Path: "/orders/{order_id}/payment-instructions",
		Summary: "Get the bank transfer instructions of an order",
		Description: "Koro's collection account, the reference code to write into the transfer description and the " +
			"amount still due, while the order awaits payment (409 orders.invalid_state otherwise). Without an active " +
			"collection account while Koro is owed money: 409 payments.no_active_collection_account. Model C adds " +
			"one separate direct transfer per payee, visible only to MFA-verified buyer administrators until direct " +
			"confirmation; each account access is audited. After a line dropped out of the order (its offering was " +
			"cancelled before payment) the amounts and the direct transfers are those of the lines left; a transfer of " +
			"the previous amount is applied up to what the order waits for and the rest is refunded as an overpayment.",
	}, op.Meta{Action: string(authz.OrdersOrderPaymentInstructions), Surface: op.SurfaceResource}, h.instructions)

	op.Register(api, "orders", huma.Operation{
		OperationID: "cancelOrder", Method: http.MethodPost, Path: "/orders/{order_id}/cancel",
		Summary: "Cancel an unpaid order",
		Description: "Only an order awaiting payment (409 orders.invalid_state). Its units go back to the offerings and " +
			"its pending rights are revoked; money already paid in part waits for a refund.",
	}, op.Meta{Action: string(authz.OrdersOrderCancel), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.cancel)

	op.Register(api, "orders", huma.Operation{
		OperationID: "listOrgSales", Method: http.MethodGet, Path: "/orgs/{org_id}/sales",
		Summary: "List the sold order lines of an organization",
		Description: "The seller's lines, newest first, with the order's reference, the buyer's title and seller_status: " +
			"the order's status from this organization's own lines of it alone (Q-31, BE-141), so another seller's " +
			"dispute, refund or delivery never shows (order_status, deprecated, carries the same value). " +
			"A line with dropped_at left its unpaid order when the offering was cancelled.",
	}, op.Meta{Action: string(authz.OrdersLineListSales), Surface: op.SurfaceOrg}, h.sales)

	op.Register(api, "orders", huma.Operation{
		OperationID: "markOrderLineFulfilled", Method: http.MethodPost, Path: "/order-lines/{line_id}/mark-fulfilled",
		Summary: "Mark an order line delivered",
		Description: "The seller says the line is delivered; the buyer's dispute window (the policy's days) starts. The " +
			"order must be paid (409 orders.order_not_paid); a line that is not pending is 409 orders.invalid_state. " +
			"evidence_file_id, optional, is a completed delivery_evidence upload (a photo or a PDF) of the selling " +
			"organization (422 otherwise; 409 files.rejected or files.not_clean when the upload was rejected or found " +
			"infected, 409 files.already_attached when another record uses it): it is attached to the line for good and " +
			"the buyer's administrators, finance and platform admins see it (getOrderLineEvidence). The seller's admin " +
			"reads back order_status = seller_status, the status of its own lines (Q-31, BE-141).",
	}, op.Meta{Action: string(authz.OrdersLineMarkFulfilled), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.mark)

	op.Register(api, "orders", huma.Operation{
		OperationID: "getOrderLineEvidence", Method: http.MethodGet, Path: "/order-lines/{line_id}/evidence",
		Summary: "Get a download URL of an order line's delivery evidence",
		Description: "A URL valid for 5 minutes of the file the seller attached when marking the line delivered, for the " +
			"buying company's administrators, the selling organization's club admins and university representatives, " +
			"and finance and platform admins. A line without evidence is 404; 409 files.not_clean until the virus scan " +
			"clears the file. Every download is audited.",
	}, op.Meta{Action: string(authz.OrdersLineReadEvidence), Surface: op.SurfaceResource}, h.evidence)

	op.Register(api, "orders", huma.Operation{
		OperationID: "replaceOrderLineEvidence", Method: http.MethodPost, Path: "/order-lines/{line_id}/replace-evidence",
		Summary: "Upload the delivery evidence of an order line again",
		Description: "Only when the line's evidence was rejected after its upload (found infected or not processable: " +
			"fulfillment_evidence_file_state = rejected; 409 files.not_rejected otherwise; a line without evidence is " +
			"409 orders.invalid_state). evidence_file_id is a new completed delivery_evidence upload of the selling " +
			"organization (422 otherwise; 409 files.rejected, files.not_clean or files.already_attached). The delivery " +
			"and its dispute window do not change; the rejected file stays on record (audit). The same people as " +
			"markOrderLineFulfilled (BE-154).",
	}, op.Meta{Action: string(authz.OrdersLineMarkFulfilled), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.replaceEvidence)

	op.Register(api, "orders", huma.Operation{
		OperationID: "confirmOrderLine", Method: http.MethodPost, Path: "/order-lines/{line_id}/confirm",
		Summary:     "Confirm the delivery of an order line",
		Description: "The buyer confirms a marked line: it is fulfilled (buyer_confirmed).",
	}, op.Meta{Action: string(authz.OrdersLineConfirm), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.confirm)

	op.Register(api, "orders", huma.Operation{
		OperationID: "disputeOrderLine", Method: http.MethodPost, Path: "/order-lines/{line_id}/dispute",
		Summary: "Dispute the delivery of an order line",
		Description: "The buyer disputes a marked line inside its dispute window (after it: 409 " +
			"orders.dispute_window_closed); a platform or finance admin resolves it.",
	}, op.Meta{Action: string(authz.OrdersLineDispute), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.dispute)

	op.Register(api, "orders", huma.Operation{
		OperationID: "adminResolveOrderLineDispute", Method: http.MethodPost, Path: "/admin/order-lines/{line_id}/resolve-dispute",
		Summary: "Resolve a disputed order line",
		Description: "fulfilled: the line is delivered (admin). refund: the line is cancelled and all the money Koro holds " +
			"of it (refund_amount, when given, must be that amount: no partial refund) waits for a refund that a finance " +
			"admin opens under four eyes.",
	}, op.Meta{Action: string(authz.OrdersLineResolveDispute), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.resolve)

	op.Register(api, "orders", huma.Operation{
		OperationID: "adminListOrders", Method: http.MethodGet, Path: "/admin/orders",
		Summary:     "List every order",
		Description: "Newest placement first; order_no takes the reference code however it was typed.",
	}, op.Meta{Action: string(authz.OrdersOrderListAdmin), Surface: op.SurfaceAdmin}, h.adminList)
}

type handler struct{ svc Service }

func i64(v int64) *int64 { return &v }

// ---- DTOs (04 §2.15, §C.2)

// SellerRefDTO names the seller of a line.
type SellerRefDTO struct {
	OrgID string `json:"org_id" format:"uuid"`
	Name  string `json:"name"`
}

// DeliverableDTO is one item of a line's deliverables.
type DeliverableDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// BuyerSnapshotDTO is the billed party as it was at placement.
type BuyerSnapshotDTO struct {
	Title     string `json:"title"`
	TaxOffice string `json:"tax_office,omitempty"`
	TaxNo     string `json:"tax_no,omitempty"`
	Address   string `json:"address,omitempty"`
	Email     string `json:"email,omitempty"`
}

// OrderLineDTO is one line of OrderDTO. The commission and the seller's net
// share are set in the seller view only (04 §C.2: never for the buyer).
type OrderLineDTO struct {
	LineID              string           `json:"line_id" format:"uuid"`
	OfferingID          string           `json:"offering_id" format:"uuid"`
	EventID             string           `json:"event_id,omitempty" format:"uuid"`
	TitleSnapshot       string           `json:"title_snapshot"`
	Deliverables        []DeliverableDTO `json:"deliverables"`
	Quantity            int              `json:"quantity"`
	UnitPriceAmount     int64            `json:"unit_price_amount" doc:"Kuruş, excluding VAT"`
	NetAmount           int64            `json:"net_amount"`
	VATRateBP           int              `json:"vat_rate_bp"`
	VATAmount           int64            `json:"vat_amount"`
	VATWithheldAmount   int64            `json:"vat_withheld_amount"`
	GrossAmount         int64            `json:"gross_amount"`
	BuyerFeeAmount      int64            `json:"buyer_fee_amount" doc:"The commission and its VAT when the policy charges the buyer, else 0"`
	PayableAmount       int64            `json:"payable_amount"`
	SellerNetAmount     *int64           `json:"seller_net_amount,omitempty" doc:"Seller view only: the payee's share"`
	CommissionAmount    *int64           `json:"commission_amount,omitempty" doc:"Seller view only"`
	CommissionVATAmount *int64           `json:"commission_vat_amount,omitempty" doc:"Seller view only"`
	FulfillmentStatus   string           `json:"fulfillment_status"`
	DisputeWindowEndsAt *time.Time       `json:"dispute_window_ends_at,omitempty"`
	FulfilledAt         *time.Time       `json:"fulfilled_at,omitempty"`
	FulfilledVia        string           `json:"fulfilled_via,omitempty"`
	HasEvidence         bool             `json:"has_fulfillment_evidence" doc:"The seller attached a delivery evidence file (getOrderLineEvidence)"`
	EvidenceFileState   string           `json:"fulfillment_evidence_file_state,omitempty" doc:"The state of the delivery evidence file (BE-154). rejected: found infected or not processable after its upload, it cannot be downloaded (409); the seller uploads it again (replaceOrderLineEvidence). pending: not scanned yet"`
	DroppedAt           *time.Time       `json:"dropped_at,omitempty" doc:"Set when the line left the order before payment because its offering was cancelled (fulfillment_status cancelled): its amounts stay as they were, but the order's amounts no longer include them and nothing is paid, refunded or delivered for it"`
	Seller              SellerRefDTO     `json:"seller"`
}

// OrderDTO is 04 §C.2 OrderDTO (view buyer: the buyer's members and staff)
// or SellerOrderView (view seller, 03 k26: order_no, buyer_title,
// placed_at, status, model_code, paid_at and the seller's own lines only;
// model_code and paid_at since BE-127; in the seller view status is
// seller_status, the status of the seller's own lines, Q-31, BE-141).
type OrderDTO struct {
	OrderID             string            `json:"order_id" format:"uuid"`
	OrderNo             string            `json:"order_no" doc:"The bank transfer reference code"`
	View                string            `json:"view" enum:"buyer,seller"`
	Status              string            `json:"status" doc:"completed: every line is final (fulfilled, cancelled, or its service refunded in full); a completed order may have been refunded in part (refunded_amount, the lines' refunded_amount). partially_refunded: part of the money went back while a line is still to deliver; the order completes once every line is final. refunded: all of the money went back. Seller view: the same value as seller_status (the seller's own lines only, never another seller's dispute or refund; Q-31)"`
	SellerStatus        string            `json:"seller_status,omitempty" doc:"Seller view only (Q-31, BE-141): the order's status computed from the reader's own lines alone, by the rule of status. awaiting_payment, expired, cancelled and refunded (all of the order's money went back) are the whole order's; cancelled also when every one of the seller's lines dropped out of the unpaid order. Otherwise from the seller's lines: completed (each final, one fulfilled), partially_refunded (one of them refunded in part), refunded (all of what Koro holds of them went back), disputed, in_fulfilment, paid. Another seller's lines, amounts, disputes and refunds never show"`
	Currency            string            `json:"currency"`
	PlacedAt            time.Time         `json:"placed_at"`
	BuyerTitle          string            `json:"buyer_title,omitempty" doc:"Seller view: the billed company's title"`
	OrgID               string            `json:"org_id,omitempty" format:"uuid" doc:"Buyer view: the buying company"`
	ModelCode           string            `json:"model_code,omitempty" doc:"Both views. C: the buyer pays each payee directly (the seller's payee receives its share from the buyer, not in a Koro payout)"`
	SubtotalAmount      *int64            `json:"subtotal_amount,omitempty"`
	VATAmount           *int64            `json:"vat_amount,omitempty"`
	VATWithheldAmount   *int64            `json:"vat_withheld_amount,omitempty"`
	BuyerFeeAmount      *int64            `json:"buyer_fee_amount,omitempty"`
	TotalAmount         *int64            `json:"total_amount,omitempty"`
	PayableAmount       *int64            `json:"payable_amount,omitempty"`
	PaidAmount          *int64            `json:"paid_amount,omitempty"`
	RefundedAmount      *int64            `json:"refunded_amount,omitempty"`
	ExpiresAt           *time.Time        `json:"expires_at,omitempty"`
	PaidAt              *time.Time        `json:"paid_at,omitempty" doc:"Both views: when the order was paid in full"`
	CompletedAt         *time.Time        `json:"completed_at,omitempty"`
	CancelledAt         *time.Time        `json:"cancelled_at,omitempty"`
	BuyerSnapshot       *BuyerSnapshotDTO `json:"buyer_snapshot,omitempty"`
	DirectStatus        string            `json:"direct_payments_status,omitempty" enum:"available,unavailable" doc:"Present only for a reader who may see the direct transfers: an MFA-verified buyer administrator reading an unpaid, unconfirmed model C order. The order never carries a payee bank account (BE-127): the transfers are in getOrderPaymentInstructions. available: the instructions list every payee transfer. unavailable: a payee has no approved bank account now (revoked or not yet approved; the instructions answer 409 parties.no_approved_account) or cannot be paid directly (409 orders.invalid_state); the rest of the order reads as usual"`
	KoroCollectAmount   *int64            `json:"koro_collect_amount,omitempty"`
	DirectToPayeeAmount *int64            `json:"direct_to_payee_amount,omitempty"`
	Lines               []OrderLineDTO    `json:"lines"`
}

func addressText(a SnapshotAddress) string {
	out := ""
	for _, p := range []string{a.Line1, a.District, a.City, a.PostalCode, a.Country} {
		if p == "" {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += p
	}
	return out
}

func orderDTO(v OrderView) OrderDTO {
	o := v.Order
	out := OrderDTO{OrderID: o.ID.String(), OrderNo: o.OrderNo, View: "buyer", Status: string(o.Status), Currency: o.Currency,
		PlacedAt: o.PlacedAt.UTC(), Lines: make([]OrderLineDTO, 0, len(v.Lines))}
	out.ModelCode, out.PaidAt = o.ModelCode, db.UTCPtr(o.PaidAt)
	if v.SellerView {
		// 03 k26 (BE-127): how and when the order was paid, never the
		// amounts or the buyer's payment details; the status of the seller's
		// own lines, never the order's (Q-31, BE-141).
		out.View, out.BuyerTitle = "seller", o.BuyerSnapshot.Title
		out.Status, out.SellerStatus = string(v.SellerStatus), string(v.SellerStatus)
	} else {
		b := o.BuyerSnapshot
		out.OrgID = o.OrgID.String()
		out.SubtotalAmount, out.VATAmount, out.VATWithheldAmount = i64(o.SubtotalAmount), i64(o.VATAmount), i64(o.VATWithheldAmount)
		out.BuyerFeeAmount, out.TotalAmount, out.PayableAmount = i64(o.BuyerFeeAmount), i64(o.TotalAmount), i64(o.PayableAmount)
		out.DirectStatus = string(v.DirectStatus)
		out.KoroCollectAmount, out.DirectToPayeeAmount = i64(o.KoroCollectAmount), i64(o.DirectToPayeeAmount)
		out.PaidAmount, out.RefundedAmount = i64(o.PaidAmount), i64(o.RefundedAmount)
		exp := o.ExpiresAt.UTC()
		out.ExpiresAt, out.CompletedAt, out.CancelledAt = &exp, db.UTCPtr(o.CompletedAt), db.UTCPtr(o.CancelledAt)
		out.BuyerSnapshot = &BuyerSnapshotDTO{Title: b.Title, TaxOffice: b.TaxOffice, TaxNo: b.TaxNo, Address: addressText(b.Address), Email: b.Email}
	}
	for _, l := range v.Lines {
		d := OrderLineDTO{
			LineID: l.ID.String(), OfferingID: l.OfferingID.String(), EventID: httpx.IDStr(l.EventID), TitleSnapshot: l.TitleSnapshot,
			Deliverables: make([]DeliverableDTO, len(l.DeliverablesSnapshot)), Quantity: l.Quantity,
			UnitPriceAmount: l.UnitPriceAmount, NetAmount: l.NetAmount, VATRateBP: l.VATRateBP, VATAmount: l.VATAmount,
			VATWithheldAmount: l.VATWithheldAmount, GrossAmount: l.GrossAmount, BuyerFeeAmount: l.BuyerFeeAmount,
			PayableAmount: l.PayableAmount, FulfillmentStatus: string(l.FulfillmentStatus),
			DisputeWindowEndsAt: db.UTCPtr(l.DisputeWindowEndsAt), FulfilledAt: db.UTCPtr(l.FulfilledAt),
			HasEvidence: l.FulfillmentEvidenceFileID != nil, DroppedAt: db.UTCPtr(l.DroppedAt),
			EvidenceFileState: string(files.StateOf(v.EvidenceStates, l.FulfillmentEvidenceFileID)),
			Seller:            SellerRefDTO{OrgID: l.SellerOrgID.String(), Name: v.Sellers[l.SellerOrgID]},
		}
		for i, x := range l.DeliverablesSnapshot {
			d.Deliverables[i] = DeliverableDTO(x)
		}
		if l.FulfilledVia != nil {
			d.FulfilledVia = string(*l.FulfilledVia)
		}
		if v.SellerView {
			d.SellerNetAmount, d.CommissionAmount, d.CommissionVATAmount = i64(l.SellerNetAmount), i64(l.CommissionAmount), i64(l.CommissionVATAmount)
		}
		out.Lines = append(out.Lines, d)
	}
	return out
}

// OrderCardDTO is one order of the order lists.
type OrderCardDTO struct {
	OrderID       string    `json:"order_id" format:"uuid"`
	OrderNo       string    `json:"order_no"`
	OrgID         string    `json:"org_id" format:"uuid"`
	BuyerTitle    string    `json:"buyer_title"`
	Status        string    `json:"status" doc:"As OrderDTO.status: a completed order may have been refunded in part"`
	ModelCode     string    `json:"model_code"`
	Currency      string    `json:"currency"`
	PayableAmount int64     `json:"payable_amount"`
	PaidAmount    int64     `json:"paid_amount"`
	PlacedAt      time.Time `json:"placed_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func cardDTO(o Order) OrderCardDTO {
	return OrderCardDTO{OrderID: o.ID.String(), OrderNo: o.OrderNo, OrgID: o.OrgID.String(), BuyerTitle: o.BuyerSnapshot.Title,
		Status: string(o.Status), ModelCode: o.ModelCode, Currency: o.Currency, PayableAmount: o.PayableAmount,
		PaidAmount: o.PaidAmount, PlacedAt: o.PlacedAt.UTC(), ExpiresAt: o.ExpiresAt.UTC()}
}

// OrgPath is the org_id path parameter.
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
}

// OrderPath is the order_id path parameter.
type OrderPath struct {
	OrderID string `path:"order_id" doc:"Order ID (an unknown or malformed ID is 404)"`
}

// LinePath is the line_id path parameter.
type LinePath struct {
	LineID string `path:"line_id" doc:"Order line ID (an unknown or malformed ID is 404)"`
}

// ---- createOrder

// OrderLineBody is one line of createOrder: an offering and a quantity,
// never a price (the server computes every amount).
type OrderLineBody struct {
	OfferingID string `json:"offering_id" format:"uuid"`
	Quantity   int    `json:"quantity" minimum:"1" maximum:"1000"`
}

// CreateOrderInputBody is the cart: the body of createOrder and of its
// preview quoteOrder (named so that both share the published schema).
type CreateOrderInputBody struct {
	BuyerPartyID string          `json:"buyer_party_id" format:"uuid" doc:"A verified buyer legal party of the company"`
	Lines        []OrderLineBody `json:"lines" minItems:"1" maxItems:"20" doc:"One line per offering"`
}

// input is the service's CreateInput of the cart (UUIDs parsed).
func (b CreateOrderInputBody) input() (CreateInput, error) {
	party, err := uuid.Parse(b.BuyerPartyID)
	if err != nil {
		return CreateInput{}, problem.Field("body.buyer_party_id", "must be a UUID")
	}
	lines := make([]LineInput, len(b.Lines))
	for i, l := range b.Lines {
		id, err := uuid.Parse(l.OfferingID)
		if err != nil {
			return CreateInput{}, problem.Field("body.lines.offering_id", "must be a UUID")
		}
		lines[i] = LineInput{OfferingID: id, Quantity: l.Quantity}
	}
	return CreateInput{BuyerPartyID: party, Lines: lines}, nil
}

type createOrderInput struct {
	idempotency.RequiredHeader
	OrgPath
	Body CreateOrderInputBody
}

type createOrderOutput struct {
	Body struct {
		Orders []OrderDTO `json:"orders" doc:"One order per money model of the lines (T-12)"`
	}
}

func (h handler) create(ctx context.Context, in *createOrderInput) (*createOrderOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ci, err := in.Body.input()
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.CreateOrder(ctx, a, httpx.ParseID(in.OrgID), ci)
	if err != nil {
		return nil, err
	}
	out := &createOrderOutput{}
	out.Body.Orders = make([]OrderDTO, len(vs))
	for i, v := range vs {
		out.Body.Orders[i] = orderDTO(v)
	}
	return out, nil
}

// ---- quoteOrder

// OrderQuoteLineDTO is one line of OrderQuoteDTO: the line createOrder
// would freeze, with the fields of OrderLineDTO that exist before
// placement (no line ID, delivery or evidence). Never the commission.
type OrderQuoteLineDTO struct {
	OfferingID        string           `json:"offering_id" format:"uuid"`
	EventID           string           `json:"event_id,omitempty" format:"uuid"`
	TitleSnapshot     string           `json:"title_snapshot" doc:"The offering's title, as createOrder would freeze it on the line"`
	Deliverables      []DeliverableDTO `json:"deliverables"`
	Seller            SellerRefDTO     `json:"seller"`
	Quantity          int              `json:"quantity"`
	UnitPriceAmount   int64            `json:"unit_price_amount" doc:"Kuruş, excluding VAT"`
	NetAmount         int64            `json:"net_amount"`
	VATRateBP         int              `json:"vat_rate_bp" doc:"The rate the line is taxed at: 0 for an exempt or out-of-scope service"`
	VATAmount         int64            `json:"vat_amount"`
	VATWithheldAmount int64            `json:"vat_withheld_amount" doc:"The VAT the buyer withholds (tevkifat) and does not transfer"`
	GrossAmount       int64            `json:"gross_amount"`
	BuyerFeeAmount    int64            `json:"buyer_fee_amount" doc:"The commission and its VAT when the policy charges the buyer, else 0"`
	PayableAmount     int64            `json:"payable_amount"`
}

// OrderQuoteDTO is one order createOrder would place now: its money model,
// totals and lines, with the names and meanings of OrderDTO's buyer view.
type OrderQuoteDTO struct {
	ModelCode           string              `json:"model_code" doc:"C: the buyer pays each payee directly and Koro its fee (koro_collect_amount)"`
	Currency            string              `json:"currency"`
	SubtotalAmount      int64               `json:"subtotal_amount"`
	VATAmount           int64               `json:"vat_amount"`
	VATWithheldAmount   int64               `json:"vat_withheld_amount"`
	BuyerFeeAmount      int64               `json:"buyer_fee_amount"`
	TotalAmount         int64               `json:"total_amount"`
	PayableAmount       int64               `json:"payable_amount"`
	KoroCollectAmount   int64               `json:"koro_collect_amount" doc:"What one transfer to Koro's account would carry"`
	DirectToPayeeAmount int64               `json:"direct_to_payee_amount" doc:"Model C: what the buyer would pay the payees directly"`
	Lines               []OrderQuoteLineDTO `json:"lines" doc:"In request order"`
}

func quoteDTO(v OrderView) OrderQuoteDTO {
	o := v.Order
	out := OrderQuoteDTO{ModelCode: o.ModelCode, Currency: o.Currency, SubtotalAmount: o.SubtotalAmount, VATAmount: o.VATAmount,
		VATWithheldAmount: o.VATWithheldAmount, BuyerFeeAmount: o.BuyerFeeAmount, TotalAmount: o.TotalAmount,
		PayableAmount: o.PayableAmount, KoroCollectAmount: o.KoroCollectAmount, DirectToPayeeAmount: o.DirectToPayeeAmount,
		Lines: make([]OrderQuoteLineDTO, len(v.Lines))}
	for i, l := range v.Lines {
		d := OrderQuoteLineDTO{
			OfferingID: l.OfferingID.String(), EventID: httpx.IDStr(l.EventID), TitleSnapshot: l.TitleSnapshot,
			Deliverables: make([]DeliverableDTO, len(l.DeliverablesSnapshot)),
			Seller:       SellerRefDTO{OrgID: l.SellerOrgID.String(), Name: v.Sellers[l.SellerOrgID]},
			Quantity:     l.Quantity, UnitPriceAmount: l.UnitPriceAmount, NetAmount: l.NetAmount, VATRateBP: l.VATRateBP,
			VATAmount: l.VATAmount, VATWithheldAmount: l.VATWithheldAmount, GrossAmount: l.GrossAmount,
			BuyerFeeAmount: l.BuyerFeeAmount, PayableAmount: l.PayableAmount,
		}
		for j, x := range l.DeliverablesSnapshot {
			d.Deliverables[j] = DeliverableDTO(x)
		}
		out.Lines[i] = d
	}
	return out
}

type quoteOrderInput struct {
	OrgPath
	Body CreateOrderInputBody
}

type quoteOrderOutput struct {
	Body struct {
		Orders []OrderQuoteDTO `json:"orders" doc:"One order per money model of the lines (T-12), as createOrder would place them now"`
	}
}

func (h handler) quote(ctx context.Context, in *quoteOrderInput) (*quoteOrderOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ci, err := in.Body.input()
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.QuoteOrder(ctx, a, httpx.ParseID(in.OrgID), ci)
	if err != nil {
		return nil, err
	}
	out := &quoteOrderOutput{}
	out.Body.Orders = make([]OrderQuoteDTO, len(vs))
	for i, v := range vs {
		out.Body.Orders[i] = quoteDTO(v)
	}
	return out, nil
}

// ---- order lists

const sortPlaced = "-placed_at"

// timeCursor decodes a (time, id) cursor of sort key sort.
func timeCursor(cursor, sort, fp string) (*TimeKey, error) {
	c, err := page.Decode(cursor, sort, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &TimeKey{At: at, ID: id}, nil
}

type orderListOutput struct {
	Body struct {
		Items      []OrderCardDTO `json:"items"`
		NextCursor string         `json:"next_cursor,omitempty"`
	}
}

func orderListOut(p OrderPage, fp string) *orderListOutput {
	out := &orderListOutput{}
	out.Body.Items = make([]OrderCardDTO, len(p.Items))
	for i, o := range p.Items {
		out.Body.Items[i] = cardDTO(o)
	}
	if k := p.Next; k != nil {
		out.Body.NextCursor = page.Next(sortPlaced, fp, page.TimeVal(k.At), page.UUIDVal(k.ID))
	}
	return out
}

type listOrgOrdersInput struct {
	OrgPath
	page.Params
	Status string `query:"status" enum:"awaiting_payment,paid,in_fulfilment,completed,cancelled,expired,partially_refunded,refunded,disputed"`
	Sort   string `query:"sort" enum:"-placed_at" doc:"-placed_at (the only order)"`
}

func (h handler) listOrg(ctx context.Context, in *listOrgOrdersInput) (*orderListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgOrders", in.OrgID, in.Status)
	after, err := timeCursor(in.Cursor, sortPlaced, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListOrgOrders(ctx, a, httpx.ParseID(in.OrgID), ListQuery{Status: Status(in.Status), Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	return orderListOut(p, fp), nil
}

type adminListOrdersInput struct {
	page.Params
	Status     string `query:"status" enum:"awaiting_payment,paid,in_fulfilment,completed,cancelled,expired,partially_refunded,refunded,disputed"`
	OrgID      string `query:"org_id" format:"uuid" doc:"The buying company"`
	OrderNo    string `query:"order_no" maxLength:"40" doc:"Reference code (spaces and hyphens ignored)"`
	PlacedFrom string `query:"placed_from" format:"date-time" doc:"placed_at >= placed_from"`
	PlacedTo   string `query:"placed_to" format:"date-time" doc:"placed_at < placed_to"`
}

func parseTime(loc, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, problem.Field(loc, "must be an RFC 3339 time")
	}
	u := t.UTC()
	return &u, nil
}

func (h handler) adminList(ctx context.Context, in *adminListOrdersInput) (*orderListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, err := httpx.OptID("query.org_id", in.OrgID)
	if err != nil {
		return nil, err
	}
	from, err := parseTime("query.placed_from", in.PlacedFrom)
	if err != nil {
		return nil, err
	}
	to, err := parseTime("query.placed_to", in.PlacedTo)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListOrders", in.Status, in.OrgID, in.OrderNo, in.PlacedFrom, in.PlacedTo)
	after, err := timeCursor(in.Cursor, sortPlaced, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListOrders(ctx, a, ListQuery{Status: Status(in.Status), OrgID: org, OrderNo: in.OrderNo, PlacedFrom: from,
		PlacedUntil: to, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	return orderListOut(p, fp), nil
}

// ---- getOrder, payment instructions, cancel

type orderOutput struct{ Body OrderDTO }

func (h handler) get(ctx context.Context, in *OrderPath) (*orderOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetOrder(ctx, a, httpx.ParseID(in.OrderID))
	if err != nil {
		return nil, err
	}
	return &orderOutput{Body: orderDTO(v)}, nil
}

// DirectPaymentDTO is one transfer to an approved legal-person payee.
type DirectPaymentDTO struct {
	PayeeID    string `json:"payee_id" format:"uuid"`
	Title      string `json:"title"`
	Amount     int64  `json:"direct_to_payee_amount"`
	IBAN       string `json:"iban"`
	HolderName string `json:"holder_name"`
	BankName   string `json:"bank_name"`
}

func directPaymentDTOs(in []DirectInstruction) []DirectPaymentDTO {
	var out []DirectPaymentDTO
	for _, v := range in {
		out = append(out, DirectPaymentDTO{PayeeID: v.PayeeID.String(), Title: v.Title,
			Amount: v.Amount, IBAN: v.IBAN, HolderName: v.HolderName, BankName: v.BankName})
	}
	return out
}

// PaymentInstructionsDTO is 04 §C.3.
type PaymentInstructionsDTO struct {
	DirectPayments  []DirectPaymentDTO `json:"direct_payments,omitempty" doc:"Separate payee transfers; buyer administrators only; hidden after direct confirmation"`
	ReferenceCode   string             `json:"reference_code"`
	AmountDueAmount int64              `json:"amount_due_amount" doc:"Kuruş still due to Koro's account"`
	Currency        string             `json:"currency"`
	IBAN            string             `json:"iban"`
	HolderName      string             `json:"holder_name"`
	BankName        string             `json:"bank_name"`
	ExpiresAt       time.Time          `json:"expires_at"`
	InstructionsTR  string             `json:"instructions_tr"`
}

type instructionsOutput struct{ Body PaymentInstructionsDTO }

func (h handler) instructions(ctx context.Context, in *OrderPath) (*instructionsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.PaymentInstructions(ctx, a, httpx.ParseID(in.OrderID))
	if err != nil {
		return nil, err
	}
	return &instructionsOutput{Body: PaymentInstructionsDTO{
		DirectPayments: directPaymentDTOs(v.DirectPayments), ReferenceCode: v.ReferenceCode, AmountDueAmount: v.AmountDue, Currency: v.Currency, IBAN: v.IBAN,
		HolderName: v.HolderName, BankName: v.BankName, ExpiresAt: v.ExpiresAt.UTC(),
		InstructionsTR: "Havale açıklamasına yalnız referans kodunu yazın: " + v.ReferenceCode,
	}}, nil
}

type cancelOrderInput struct {
	idempotency.OptionalHeader
	OrderPath
	Body struct {
		Reason string `json:"reason" minLength:"1" maxLength:"500"`
	}
}

func (h handler) cancel(ctx context.Context, in *cancelOrderInput) (*orderOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CancelOrder(ctx, a, httpx.ParseID(in.OrderID), in.Body.Reason)
	if err != nil {
		return nil, err
	}
	return &orderOutput{Body: orderDTO(v)}, nil
}

// ---- sales

// SellerLineDTO is 04 §2.15 SellerLineDTO (+ the order's status as the
// seller sees it: seller_status, Q-31, BE-141; order_status is the same
// value, deprecated).
type SellerLineDTO struct {
	LineID              string     `json:"line_id" format:"uuid"`
	OrderID             string     `json:"order_id" format:"uuid"`
	OrderNo             string     `json:"order_no"`
	SellerStatus        string     `json:"seller_status" doc:"The order's status as this organization sees it (Q-31, BE-141): from all of its own lines of the order, never another seller's (OrderDTO.seller_status has the rule). A line is delivered only once its order is paid (not awaiting_payment)"`
	OrderStatus         string     `json:"order_status" deprecated:"true" doc:"Deprecated (BE-141): the same value as seller_status, the status of this organization's own lines of the order, not the order's own status (a seller never sees another seller's dispute or refund, Q-31); read seller_status. Kept for existing clients"`
	Currency            string     `json:"currency" doc:"The order's currency (ISO 4217) of the amounts"`
	BuyerTitle          string     `json:"buyer_title"`
	TitleSnapshot       string     `json:"title_snapshot"`
	Quantity            int        `json:"quantity"`
	GrossAmount         int64      `json:"gross_amount"`
	SellerNetAmount     int64      `json:"seller_net_amount"`
	FulfillmentStatus   string     `json:"fulfillment_status"`
	DisputeWindowEndsAt *time.Time `json:"dispute_window_ends_at,omitempty"`
	PaidOutAt           *time.Time `json:"paid_out_at,omitempty"`
	HasEvidence         bool       `json:"has_fulfillment_evidence" doc:"A delivery evidence file is attached (getOrderLineEvidence)"`
	EvidenceFileState   string     `json:"fulfillment_evidence_file_state,omitempty" doc:"The state of the delivery evidence file (BE-154). rejected: found infected or not processable after its upload, it cannot be downloaded (409); the seller uploads it again (replaceOrderLineEvidence). pending: not scanned yet"`
	DroppedAt           *time.Time `json:"dropped_at,omitempty" doc:"Set when the line left its order before payment because the offering was cancelled (fulfillment_status cancelled): nothing is paid for it"`
}

type salesInput struct {
	OrgPath
	page.Params
	FulfillmentStatus string `query:"fulfillment_status" enum:"not_applicable,pending,marked,fulfilled,disputed,cancelled"`
}

type salesOutput struct {
	Body struct {
		Items      []SellerLineDTO `json:"items"`
		NextCursor string          `json:"next_cursor,omitempty"`
	}
}

const sortCreated = "-created_at"

func (h handler) sales(ctx context.Context, in *salesInput) (*salesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgSales", in.OrgID, in.FulfillmentStatus)
	after, err := timeCursor(in.Cursor, sortCreated, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListOrgSales(ctx, a, httpx.ParseID(in.OrgID), SalesQuery{Fulfillment: Fulfillment(in.FulfillmentStatus), Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &salesOutput{}
	out.Body.Items = make([]SellerLineDTO, len(p.Items))
	for i, it := range p.Items {
		l := it.Line
		out.Body.Items[i] = SellerLineDTO{LineID: l.ID.String(), OrderID: l.OrderID.String(), OrderNo: it.OrderNo,
			SellerStatus: string(it.SellerStatus), OrderStatus: string(it.SellerStatus), Currency: it.Currency, BuyerTitle: it.BuyerTitle,
			TitleSnapshot: l.TitleSnapshot, Quantity: l.Quantity,
			GrossAmount: l.GrossAmount, SellerNetAmount: l.SellerNetAmount, FulfillmentStatus: string(l.FulfillmentStatus),
			DisputeWindowEndsAt: db.UTCPtr(l.DisputeWindowEndsAt), PaidOutAt: db.UTCPtr(l.PaidOutAt),
			HasEvidence: l.FulfillmentEvidenceFileID != nil, DroppedAt: db.UTCPtr(l.DroppedAt),
			EvidenceFileState: string(files.StateOf(p.EvidenceStates, l.FulfillmentEvidenceFileID))}
	}
	if k := p.Next; k != nil {
		out.Body.NextCursor = page.Next(sortCreated, fp, page.TimeVal(k.At), page.UUIDVal(k.ID))
	}
	return out, nil
}

// ---- delivery

// LineStateDTO is a line after a delivery action.
type LineStateDTO struct {
	LineID              string     `json:"line_id" format:"uuid"`
	OrderID             string     `json:"order_id" format:"uuid"`
	OrderNo             string     `json:"order_no"`
	OrderStatus         string     `json:"order_status" doc:"The order's status; when the seller's admin marks its line (markOrderLineFulfilled), the same value as seller_status: the seller's own lines only (Q-31, BE-141)"`
	SellerStatus        string     `json:"seller_status,omitempty" doc:"Set when the seller's admin marks its line: the order's status from that seller's own lines (OrderDTO.seller_status has the rule)"`
	FulfillmentStatus   string     `json:"fulfillment_status"`
	DisputeWindowEndsAt *time.Time `json:"dispute_window_ends_at,omitempty"`
	FulfilledAt         *time.Time `json:"fulfilled_at,omitempty"`
	FulfilledVia        string     `json:"fulfilled_via,omitempty"`
	HasEvidence         bool       `json:"has_fulfillment_evidence" doc:"A delivery evidence file is attached (getOrderLineEvidence)"`
	EvidenceFileState   string     `json:"fulfillment_evidence_file_state,omitempty" doc:"The state of the delivery evidence file (BE-154). rejected: found infected or not processable after its upload, it cannot be downloaded (409); the seller uploads it again (replaceOrderLineEvidence). pending: not scanned yet"`
}

type lineOutput struct{ Body LineStateDTO }

func lineOut(v LineView) *lineOutput {
	l := v.Line
	d := LineStateDTO{LineID: l.ID.String(), OrderID: l.OrderID.String(), OrderNo: v.OrderNo, OrderStatus: string(v.OrderStatus),
		FulfillmentStatus: string(l.FulfillmentStatus), DisputeWindowEndsAt: db.UTCPtr(l.DisputeWindowEndsAt), FulfilledAt: db.UTCPtr(l.FulfilledAt),
		HasEvidence: l.FulfillmentEvidenceFileID != nil, EvidenceFileState: string(v.EvidenceState)}
	if v.SellerView {
		d.SellerStatus = string(v.OrderStatus)
	}
	if l.FulfilledVia != nil {
		d.FulfilledVia = string(*l.FulfilledVia)
	}
	return &lineOutput{Body: d}
}

type markInput struct {
	idempotency.OptionalHeader
	LinePath
	Body *struct {
		Note           string `json:"note,omitempty" maxLength:"500"`
		EvidenceFileID string `json:"evidence_file_id,omitempty" format:"uuid" doc:"A completed delivery_evidence upload (photo or PDF) of the selling organization, attached to the line"`
	}
}

func (h handler) mark(ctx context.Context, in *markInput) (*lineOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var mi MarkInput
	if in.Body != nil {
		mi.Note = in.Body.Note
		if mi.EvidenceFileID, err = httpx.OptID("body.evidence_file_id", in.Body.EvidenceFileID); err != nil {
			return nil, err
		}
	}
	v, err := h.svc.MarkLineFulfilled(ctx, a, httpx.ParseID(in.LineID), mi)
	if err != nil {
		return nil, err
	}
	return lineOut(v), nil
}

// EvidenceLinkDTO is the body of getOrderLineEvidence (04 §1.14 signed URL).
type EvidenceLinkDTO struct {
	URL       string    `json:"url" doc:"Valid for at most 5 minutes"`
	ExpiresAt time.Time `json:"expires_at"`
}

type evidenceInput struct{ LinePath }

type evidenceOutput struct{ Body EvidenceLinkDTO }

func (h handler) evidence(ctx context.Context, in *evidenceInput) (*evidenceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	dl, err := h.svc.LineEvidence(ctx, a, httpx.ParseID(in.LineID))
	if err != nil {
		return nil, err
	}
	return &evidenceOutput{Body: EvidenceLinkDTO{URL: dl.URL, ExpiresAt: dl.ExpiresAt.UTC()}}, nil
}

type replaceEvidenceInput struct {
	idempotency.OptionalHeader
	LinePath
	Body struct {
		EvidenceFileID string `json:"evidence_file_id" format:"uuid" doc:"A new completed delivery_evidence upload (photo or PDF) of the selling organization"`
	}
}

func (h handler) replaceEvidence(ctx context.Context, in *replaceEvidenceInput) (*lineOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReplaceLineEvidence(ctx, a, httpx.ParseID(in.LineID), httpx.ParseID(in.Body.EvidenceFileID))
	if err != nil {
		return nil, err
	}
	return lineOut(v), nil
}

type confirmInput struct {
	idempotency.OptionalHeader
	LinePath
}

func (h handler) confirm(ctx context.Context, in *confirmInput) (*lineOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ConfirmLine(ctx, a, httpx.ParseID(in.LineID))
	if err != nil {
		return nil, err
	}
	return lineOut(v), nil
}

type disputeInput struct {
	idempotency.OptionalHeader
	LinePath
	Body struct {
		DisputeReason string `json:"dispute_reason" minLength:"1" maxLength:"1000"`
	}
}

func (h handler) dispute(ctx context.Context, in *disputeInput) (*lineOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.DisputeLine(ctx, a, httpx.ParseID(in.LineID), in.Body.DisputeReason)
	if err != nil {
		return nil, err
	}
	return lineOut(v), nil
}

type resolveInput struct {
	idempotency.OptionalHeader
	LinePath
	Body struct {
		Outcome      string `json:"outcome" enum:"fulfilled,refund"`
		Note         string `json:"note" minLength:"1" maxLength:"1000"`
		RefundAmount *int64 `json:"refund_amount,omitempty" minimum:"1" doc:"refund only: kuruş, the line's whole refundable amount (a check; omitted = the same)"`
	}
}

func (h handler) resolve(ctx context.Context, in *resolveInput) (*lineOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminResolveDispute(ctx, a, httpx.ParseID(in.LineID),
		ResolveInput{Outcome: in.Body.Outcome, Note: in.Body.Note, RefundAmount: in.Body.RefundAmount})
	if err != nil {
		return nil, err
	}
	return lineOut(v), nil
}
