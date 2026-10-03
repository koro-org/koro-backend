package domainevent

import "uuid"

// Topics of the orders module (01 §6.2). Owner: orders tickets.
const (
	TopicOrderPlaced                = "order.placed"
	TopicOrderCancelled             = "order.cancelled"
	TopicOrderExpired               = "order.expired"
	TopicOrderLineFulfillmentMarked = "order_line.fulfillment_marked"
	TopicOrderLineFulfilled         = "order_line.fulfilled"
)

var _ = register(
	spec("orders", Outbox, OrderPlaced{}),
	spec("orders", Outbox, OrderCancelled{}),
	spec("orders", AfterCommit, OrderExpired{}),
	spec("orders", AfterCommit, OrderLineFulfillmentMarked{}),
	spec("orders", Outbox, OrderLineFulfilled{}),
)

// OrderPlaced: the buyer gets the payment instruction; ModelCode is the
// commerce model of the order.
type OrderPlaced struct {
	OrderID   uuid.UUID `json:"order_id"`
	OrgID     uuid.UUID `json:"org_id"`
	ModelCode string    `json:"model_code"`
}

// Topic implements Payload.
func (OrderPlaced) Topic() string { return TopicOrderPlaced }

// OrderCancelled: Reason is a code.
type OrderCancelled struct {
	OrderID uuid.UUID `json:"order_id"`
	Reason  string    `json:"reason"`
}

// Topic implements Payload.
func (OrderCancelled) Topic() string { return TopicOrderCancelled }

// OrderExpired: the buyer is notified.
type OrderExpired struct {
	OrderID uuid.UUID `json:"order_id"`
}

// Topic implements Payload.
func (OrderExpired) Topic() string { return TopicOrderExpired }

// OrderLineFulfillmentMarked: the buyer is asked to confirm delivery.
type OrderLineFulfillmentMarked struct {
	OrderLineID uuid.UUID `json:"order_line_id"`
	OrderID     uuid.UUID `json:"order_id"`
}

// Topic implements Payload.
func (OrderLineFulfillmentMarked) Topic() string { return TopicOrderLineFulfillmentMarked }

// OrderLineFulfilled: billing plans the on_fulfillment invoice.
type OrderLineFulfilled struct {
	OrderLineID uuid.UUID `json:"order_line_id"`
	OrderID     uuid.UUID `json:"order_id"`
	PayeeID     uuid.UUID `json:"payee_id"`
}

// Topic implements Payload.
func (OrderLineFulfilled) Topic() string { return TopicOrderLineFulfilled }
