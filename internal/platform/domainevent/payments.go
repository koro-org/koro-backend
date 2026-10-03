package domainevent

import (
	"time"
	"uuid"
)

// Topics of the payments module (01 §6.2). Owner: payments tickets.
const (
	TopicCollectionAccountApproved = "collection_account.approved"
	TopicPaymentCaptured           = "payment.captured"
	TopicPaymentRefunded           = "payment.refunded"
	TopicIncomingTransferUnmatched = "incoming_transfer.unmatched"
)

var _ = register(
	spec("payments", Outbox, CollectionAccountApproved{}),
	spec("payments", Outbox, PaymentCaptured{}),
	spec("payments", Outbox, PaymentRefunded{}),
	spec("payments", AfterCommit, IncomingTransferUnmatched{}),
)

// CollectionAccountApproved: security notification to platform admins.
type CollectionAccountApproved struct {
	CollectionAccountID uuid.UUID `json:"collection_account_id"`
}

// Topic implements Payload.
func (CollectionAccountApproved) Topic() string { return TopicCollectionAccountApproved }

// PaymentCaptured: Amount is in kuruş (int64); OrderPaid reports that the
// order is now fully paid.
type PaymentCaptured struct {
	PaymentID uuid.UUID `json:"payment_id"`
	OrderID   uuid.UUID `json:"order_id"`
	Amount    int64     `json:"amount"`
	OrderPaid bool      `json:"order_paid"`
}

// Topic implements Payload.
func (PaymentCaptured) Topic() string { return TopicPaymentCaptured }

// PaymentRefunded: Amount is in kuruş.
type PaymentRefunded struct {
	RefundID    uuid.UUID `json:"refund_id"`
	PaymentID   uuid.UUID `json:"payment_id"`
	OrderID     uuid.UUID `json:"order_id"`
	OrderLineID uuid.UUID `json:"order_line_id"`
	Amount      int64     `json:"amount"`
}

// Topic implements Payload.
func (PaymentRefunded) Topic() string { return TopicPaymentRefunded }

// IncomingTransferUnmatched: finance in-app digest.
type IncomingTransferUnmatched struct {
	Count    int       `json:"count"`
	OldestAt time.Time `json:"oldest_at"`
}

// Topic implements Payload.
func (IncomingTransferUnmatched) Topic() string { return TopicIncomingTransferUnmatched }
