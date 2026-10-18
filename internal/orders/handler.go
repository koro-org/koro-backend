package orders

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// Register adds the orders operations (04 §2.15) to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	reg(api, huma.Operation{
		OperationID: "createOrder", Method: http.MethodPost, Path: "/orgs/{org_id}/orders",
		Summary: "Place an order", DefaultStatus: http.StatusCreated,
		Description: "A company admin buys offerings for the company, billed to a verified buyer party of the company " +
			"(422 orders.buyer_party_required). Prices, VAT, withholding and commission are computed on the server from " +
			"the offering and the commerce policy in effect (none: 409 catalog.no_commerce_policy); lines under different " +
			"money models become separate orders, each with its own reference code. The stock is reserved in the same " +
			"transaction; an unpaid order expires after the payment window and releases it.",
	}, op.Meta{Action: string(authz.OrdersOrderCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyRequired}, h.create)

	reg(api, huma.Operation{
		OperationID: "listOrgOrders", Method: http.MethodGet, Path: "/orgs/{org_id}/orders",
		Summary:     "List the orders of a company",
		Description: "Newest placement first.",
	}, op.Meta{Action: string(authz.OrdersOrderList), Surface: op.SurfaceOrg}, h.listOrg)

	reg(api, huma.Operation{
		OperationID: "getOrder", Method: http.MethodGet, Path: "/orders/{order_id}",
		Summary: "Get an order",
		Description: "The buyer's members and platform staff get the whole order (view buyer: totals, payment, every " +
			"line; never the commission). Only MFA-verified buyer administrators see approved payee bank instructions " +
			"on an unpaid, unconfirmed model C order; bank access is audited. A seller's club admin or university representative gets only its " +
			"own lines with its net share and the commission, and the buyer's title (view seller). Anyone else: 404.",
	}, op.Meta{Action: string(authz.OrdersOrderRead), Surface: op.SurfaceResource}, h.get)

	reg(api, huma.Operation{
		OperationID: "getOrderPaymentInstructions", Method: http.MethodGet, Path: "/orders/{order_id}/payment-instructions",
		Summary: "Get the bank transfer instructions of an order",
		Description: "Koro's collection account, the reference code to write into the transfer description and the " +
			"amount still due, while the order awaits payment (409 orders.invalid_state otherwise). Without an active " +
			"collection account while Koro is owed money: 409 payments.no_active_collection_account. Model C adds " +
			"one separate direct transfer per payee, visible only to MFA-verified buyer administrators until direct " +
			"confirmation; each account access is audited.",
	}, op.Meta{Action: string(authz.OrdersOrderPaymentInstructions), Surface: op.SurfaceResource}, h.instructions)

	reg(api, huma.Operation{
		OperationID: "cancelOrder", Method: http.MethodPost, Path: "/orders/{order_id}/cancel",
		Summary: "Cancel an unpaid order",
		Description: "Only an order awaiting payment (409 orders.invalid_state). Its units go back to the offerings and " +
			"its pending rights are revoked; money already paid in part waits for a refund.",
	}, op.Meta{Action: string(authz.OrdersOrderCancel), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.cancel)

	reg(api, huma.Operation{
		OperationID: "listOrgSales", Method: http.MethodGet, Path: "/orgs/{org_id}/sales",
		Summary:     "List the sold order lines of an organization",
		Description: "The seller's lines, newest first, with the order's reference and status and the buyer's title.",
	}, op.Meta{Action: string(authz.OrdersLineListSales), Surface: op.SurfaceOrg}, h.sales)

	reg(api, huma.Operation{
		OperationID: "markOrderLineFulfilled", Method: http.MethodPost, Path: "/order-lines/{line_id}/mark-fulfilled",
		Summary: "Mark an order line delivered",
		Description: "The seller says the line is delivered; the buyer's dispute window (the policy's days) starts. The " +
			"order must be paid (409 orders.order_not_paid); a line that is not pending is 409 orders.invalid_state.",
	}, op.Meta{Action: string(authz.OrdersLineMarkFulfilled), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.mark)

	reg(api, huma.Operation{
		OperationID: "confirmOrderLine", Method: http.MethodPost, Path: "/order-lines/{line_id}/confirm",
		Summary:     "Confirm the delivery of an order line",
		Description: "The buyer confirms a marked line: it is fulfilled (buyer_confirmed).",
	}, op.Meta{Action: string(authz.OrdersLineConfirm), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.confirm)

	reg(api, huma.Operation{
		OperationID: "disputeOrderLine", Method: http.MethodPost, Path: "/order-lines/{line_id}/dispute",
		Summary: "Dispute the delivery of an order line",
		Description: "The buyer disputes a marked line inside its dispute window (after it: 409 " +
			"orders.dispute_window_closed); a platform or finance admin resolves it.",
	}, op.Meta{Action: string(authz.OrdersLineDispute), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.dispute)

	reg(api, huma.Operation{
		OperationID: "adminResolveOrderLineDispute", Method: http.MethodPost, Path: "/admin/order-lines/{line_id}/resolve-dispute",
		Summary: "Resolve a disputed order line",
		Description: "fulfilled: the line is delivered (admin). refund: the line is cancelled and all the money Koro holds " +
			"of it (refund_amount, when given, must be that amount: no partial refund) waits for a refund that a finance " +
			"admin opens under four eyes.",
	}, op.Meta{Action: string(authz.OrdersLineResolveDispute), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.resolve)

	reg(api, huma.Operation{
		OperationID: "adminListOrders", Method: http.MethodGet, Path: "/admin/orders",
		Summary:     "List every order",
		Description: "Newest placement first; order_no takes the reference code however it was typed.",
	}, op.Meta{Action: string(authz.OrdersOrderListAdmin), Surface: op.SurfaceAdmin}, h.adminList)
}

// reg registers one orders operation with its Koro metadata.
func reg[I, O any](api huma.API, o huma.Operation, m op.Meta, fn func(context.Context, *I) (*O, error)) {
	o.Tags = []string{"orders"}
	huma.Register(api, op.Koro(o, m), fn)
}

type handler struct{ svc Service }

// parseID parses a path ID; a malformed one becomes the nil UUID, which no
// row has: it answers like a missing row (404) after the surface's authz.
func parseID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil()
	}
	return id
}

func optID(loc, s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, fieldErr(loc, "must be a UUID")
	}
	return &id, nil
}

func idStr(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

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
	FulfillmentStatus   string           `json:"fulfillment_status" enum:"not_applicable,pending,marked,fulfilled,disputed,cancelled"`
	DisputeWindowEndsAt *time.Time       `json:"dispute_window_ends_at,omitempty"`
	FulfilledAt         *time.Time       `json:"fulfilled_at,omitempty"`
	FulfilledVia        string           `json:"fulfilled_via,omitempty" enum:"buyer_confirmed,window_elapsed,admin,auto_platform"`
	Seller              SellerRefDTO     `json:"seller"`
}

// OrderDTO is 04 §C.2 OrderDTO (view buyer: the buyer's members and staff)
// or SellerOrderView (view seller, 03 k26: order_no, buyer_title,
// placed_at, status and the seller's own lines only).
type OrderDTO struct {
	OrderID             string             `json:"order_id" format:"uuid"`
	OrderNo             string             `json:"order_no" doc:"The bank transfer reference code"`
	View                string             `json:"view" enum:"buyer,seller"`
	Status              string             `json:"status" enum:"awaiting_payment,paid,in_fulfilment,completed,cancelled,expired,partially_refunded,refunded,disputed"`
	Currency            string             `json:"currency"`
	PlacedAt            time.Time          `json:"placed_at"`
	BuyerTitle          string             `json:"buyer_title,omitempty" doc:"Seller view: the billed company's title"`
	OrgID               string             `json:"org_id,omitempty" format:"uuid" doc:"Buyer view: the buying company"`
	ModelCode           string             `json:"model_code,omitempty"`
	SubtotalAmount      *int64             `json:"subtotal_amount,omitempty"`
	VATAmount           *int64             `json:"vat_amount,omitempty"`
	VATWithheldAmount   *int64             `json:"vat_withheld_amount,omitempty"`
	BuyerFeeAmount      *int64             `json:"buyer_fee_amount,omitempty"`
	TotalAmount         *int64             `json:"total_amount,omitempty"`
	PayableAmount       *int64             `json:"payable_amount,omitempty"`
	PaidAmount          *int64             `json:"paid_amount,omitempty"`
	RefundedAmount      *int64             `json:"refunded_amount,omitempty"`
	ExpiresAt           *time.Time         `json:"expires_at,omitempty"`
	PaidAt              *time.Time         `json:"paid_at,omitempty"`
	CompletedAt         *time.Time         `json:"completed_at,omitempty"`
	CancelledAt         *time.Time         `json:"cancelled_at,omitempty"`
	BuyerSnapshot       *BuyerSnapshotDTO  `json:"buyer_snapshot,omitempty"`
	DirectPayments      []DirectPaymentDTO `json:"direct_payments,omitempty" doc:"Unpaid model C: approved payee accounts, visible only to the buying company administrators"`
	KoroCollectAmount   *int64             `json:"koro_collect_amount,omitempty"`
	DirectToPayeeAmount *int64             `json:"direct_to_payee_amount,omitempty"`
	Lines               []OrderLineDTO     `json:"lines"`
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
	if v.SellerView {
		out.View, out.BuyerTitle = "seller", o.BuyerSnapshot.Title
	} else {
		b := o.BuyerSnapshot
		out.OrgID, out.ModelCode = o.OrgID.String(), o.ModelCode
		out.SubtotalAmount, out.VATAmount, out.VATWithheldAmount = i64(o.SubtotalAmount), i64(o.VATAmount), i64(o.VATWithheldAmount)
		out.BuyerFeeAmount, out.TotalAmount, out.PayableAmount = i64(o.BuyerFeeAmount), i64(o.TotalAmount), i64(o.PayableAmount)
		out.DirectPayments = directPaymentDTOs(v.DirectPayments)
		out.KoroCollectAmount, out.DirectToPayeeAmount = i64(o.KoroCollectAmount), i64(o.DirectToPayeeAmount)
		out.PaidAmount, out.RefundedAmount = i64(o.PaidAmount), i64(o.RefundedAmount)
		exp := o.ExpiresAt.UTC()
		out.ExpiresAt, out.PaidAt, out.CompletedAt, out.CancelledAt = &exp, utcPtr(o.PaidAt), utcPtr(o.CompletedAt), utcPtr(o.CancelledAt)
		out.BuyerSnapshot = &BuyerSnapshotDTO{Title: b.Title, TaxOffice: b.TaxOffice, TaxNo: b.TaxNo, Address: addressText(b.Address), Email: b.Email}
	}
	for _, l := range v.Lines {
		d := OrderLineDTO{
			LineID: l.ID.String(), OfferingID: l.OfferingID.String(), EventID: idStr(l.EventID), TitleSnapshot: l.TitleSnapshot,
			Deliverables: make([]DeliverableDTO, len(l.DeliverablesSnapshot)), Quantity: l.Quantity,
			UnitPriceAmount: l.UnitPriceAmount, NetAmount: l.NetAmount, VATRateBP: l.VATRateBP, VATAmount: l.VATAmount,
			VATWithheldAmount: l.VATWithheldAmount, GrossAmount: l.GrossAmount, BuyerFeeAmount: l.BuyerFeeAmount,
			PayableAmount: l.PayableAmount, FulfillmentStatus: string(l.FulfillmentStatus),
			DisputeWindowEndsAt: utcPtr(l.DisputeWindowEndsAt), FulfilledAt: utcPtr(l.FulfilledAt),
			Seller: SellerRefDTO{OrgID: l.SellerOrgID.String(), Name: v.Sellers[l.SellerOrgID]},
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
	Status        string    `json:"status"`
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

type createOrderInput struct {
	idempotency.RequiredHeader
	OrgPath
	Body struct {
		BuyerPartyID string          `json:"buyer_party_id" format:"uuid" doc:"A verified buyer legal party of the company"`
		Lines        []OrderLineBody `json:"lines" minItems:"1" maxItems:"20" doc:"One line per offering"`
	}
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
	party, err := uuid.Parse(in.Body.BuyerPartyID)
	if err != nil {
		return nil, fieldErr("body.buyer_party_id", "must be a UUID")
	}
	lines := make([]LineInput, len(in.Body.Lines))
	for i, l := range in.Body.Lines {
		id, err := uuid.Parse(l.OfferingID)
		if err != nil {
			return nil, fieldErr("body.lines.offering_id", "must be a UUID")
		}
		lines[i] = LineInput{OfferingID: id, Quantity: l.Quantity}
	}
	vs, err := h.svc.CreateOrder(ctx, a, parseID(in.OrgID), CreateInput{BuyerPartyID: party, Lines: lines})
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
	p, err := h.svc.ListOrgOrders(ctx, a, parseID(in.OrgID), ListQuery{Status: Status(in.Status), Limit: in.Limit, After: after})
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
		return nil, fieldErr(loc, "must be an RFC 3339 time")
	}
	u := t.UTC()
	return &u, nil
}

func (h handler) adminList(ctx context.Context, in *adminListOrdersInput) (*orderListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	org, err := optID("query.org_id", in.OrgID)
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
	v, err := h.svc.GetOrder(ctx, a, parseID(in.OrderID))
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
	v, err := h.svc.PaymentInstructions(ctx, a, parseID(in.OrderID))
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
	v, err := h.svc.CancelOrder(ctx, a, parseID(in.OrderID), in.Body.Reason)
	if err != nil {
		return nil, err
	}
	return &orderOutput{Body: orderDTO(v)}, nil
}

// ---- sales

// SellerLineDTO is 04 §2.15 SellerLineDTO (+ the order's status).
type SellerLineDTO struct {
	LineID              string     `json:"line_id" format:"uuid"`
	OrderID             string     `json:"order_id" format:"uuid"`
	OrderNo             string     `json:"order_no"`
	OrderStatus         string     `json:"order_status" doc:"A line is delivered only once its order is paid"`
	BuyerTitle          string     `json:"buyer_title"`
	TitleSnapshot       string     `json:"title_snapshot"`
	Quantity            int        `json:"quantity"`
	GrossAmount         int64      `json:"gross_amount"`
	SellerNetAmount     int64      `json:"seller_net_amount"`
	FulfillmentStatus   string     `json:"fulfillment_status"`
	DisputeWindowEndsAt *time.Time `json:"dispute_window_ends_at,omitempty"`
	PaidOutAt           *time.Time `json:"paid_out_at,omitempty"`
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
	p, err := h.svc.ListOrgSales(ctx, a, parseID(in.OrgID), SalesQuery{Fulfillment: Fulfillment(in.FulfillmentStatus), Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &salesOutput{}
	out.Body.Items = make([]SellerLineDTO, len(p.Items))
	for i, it := range p.Items {
		l := it.Line
		out.Body.Items[i] = SellerLineDTO{LineID: l.ID.String(), OrderID: l.OrderID.String(), OrderNo: it.OrderNo,
			OrderStatus: string(it.OrderStatus), BuyerTitle: it.BuyerTitle, TitleSnapshot: l.TitleSnapshot, Quantity: l.Quantity,
			GrossAmount: l.GrossAmount, SellerNetAmount: l.SellerNetAmount, FulfillmentStatus: string(l.FulfillmentStatus),
			DisputeWindowEndsAt: utcPtr(l.DisputeWindowEndsAt), PaidOutAt: utcPtr(l.PaidOutAt)}
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
	OrderStatus         string     `json:"order_status"`
	FulfillmentStatus   string     `json:"fulfillment_status"`
	DisputeWindowEndsAt *time.Time `json:"dispute_window_ends_at,omitempty"`
	FulfilledAt         *time.Time `json:"fulfilled_at,omitempty"`
	FulfilledVia        string     `json:"fulfilled_via,omitempty"`
}

type lineOutput struct{ Body LineStateDTO }

func lineOut(v LineView) *lineOutput {
	l := v.Line
	d := LineStateDTO{LineID: l.ID.String(), OrderID: l.OrderID.String(), OrderNo: v.OrderNo, OrderStatus: string(v.OrderStatus),
		FulfillmentStatus: string(l.FulfillmentStatus), DisputeWindowEndsAt: utcPtr(l.DisputeWindowEndsAt), FulfilledAt: utcPtr(l.FulfilledAt)}
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
		EvidenceFileID string `json:"evidence_file_id,omitempty" format:"uuid" doc:"An uploaded file showing the delivery (recorded with the audit trail)"`
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
		if mi.EvidenceFileID, err = optID("body.evidence_file_id", in.Body.EvidenceFileID); err != nil {
			return nil, err
		}
	}
	v, err := h.svc.MarkLineFulfilled(ctx, a, parseID(in.LineID), mi)
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
	v, err := h.svc.ConfirmLine(ctx, a, parseID(in.LineID))
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
	v, err := h.svc.DisputeLine(ctx, a, parseID(in.LineID), in.Body.DisputeReason)
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
	v, err := h.svc.AdminResolveDispute(ctx, a, parseID(in.LineID),
		ResolveInput{Outcome: in.Body.Outcome, Note: in.Body.Note, RefundAmount: in.Body.RefundAmount})
	if err != nil {
		return nil, err
	}
	return lineOut(v), nil
}
