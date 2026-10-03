package domainevent

import "uuid"

// Topics of the ledger module (01 §6.2). Owner: ledger tickets.
const (
	TopicPayoutCreated  = "payout.created"
	TopicPayoutApproved = "payout.approved"
	TopicPayoutPaid     = "payout.paid"
)

var _ = register(
	spec("ledger", AfterCommit, PayoutCreated{}),
	spec("ledger", Outbox, PayoutApproved{}),
	spec("ledger", Outbox, PayoutPaid{}),
)

// PayoutCreated: the other finance admins see "awaiting approval".
type PayoutCreated struct {
	PayoutID uuid.UUID `json:"payout_id"`
	PayeeID  uuid.UUID `json:"payee_id"`
}

// Topic implements Payload.
func (PayoutCreated) Topic() string { return TopicPayoutCreated }

// PayoutApproved: the finance admin making the transfer is notified.
type PayoutApproved struct {
	PayoutID uuid.UUID `json:"payout_id"`
}

// Topic implements Payload.
func (PayoutApproved) Topic() string { return TopicPayoutApproved }

// PayoutPaid: Amount is in kuruş.
type PayoutPaid struct {
	PayoutID uuid.UUID `json:"payout_id"`
	PayeeID  uuid.UUID `json:"payee_id"`
	Amount   int64     `json:"amount"`
}

// Topic implements Payload.
func (PayoutPaid) Topic() string { return TopicPayoutPaid }
