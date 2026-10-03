package domainevent

import "uuid"

// Topics of the parties module (01 §6.2). Owner: parties tickets.
const (
	TopicPayeeBankAccountSubmitted = "payee_bank_account.submitted"
	TopicPayeeBankAccountApproved  = "payee_bank_account.approved"
	TopicPayeeBankAccountRejected  = "payee_bank_account.rejected"
)

var _ = register(
	spec("parties", Outbox, PayeeBankAccountSubmitted{}),
	spec("parties", Outbox, PayeeBankAccountApproved{}),
	spec("parties", AfterCommit, PayeeBankAccountRejected{}),
)

// PayeeBankAccountSubmitted: finance admins see "awaiting approval". The
// IBAN never enters the event.
type PayeeBankAccountSubmitted struct {
	BankAccountID uuid.UUID `json:"bank_account_id"`
	PayeeID       uuid.UUID `json:"payee_id"`
	Stage         string    `json:"stage"`
}

// Topic implements Payload.
func (PayeeBankAccountSubmitted) Topic() string { return TopicPayeeBankAccountSubmitted }

// PayeeBankAccountApproved: security notification to the payee org admins
// and every finance admin.
type PayeeBankAccountApproved struct {
	BankAccountID uuid.UUID `json:"bank_account_id"`
	PayeeID       uuid.UUID `json:"payee_id"`
}

// Topic implements Payload.
func (PayeeBankAccountApproved) Topic() string { return TopicPayeeBankAccountApproved }

// PayeeBankAccountRejected: the submitter is notified.
type PayeeBankAccountRejected struct {
	BankAccountID uuid.UUID `json:"bank_account_id"`
	PayeeID       uuid.UUID `json:"payee_id"`
}

// Topic implements Payload.
func (PayeeBankAccountRejected) Topic() string { return TopicPayeeBankAccountRejected }
