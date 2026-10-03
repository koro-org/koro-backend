package domainevent

import "uuid"

// Topics of the billing module (01 §6.2). Owner: billing tickets.
const (
	TopicInvoiceRequested = "invoice.requested"
	TopicInvoiceIssued    = "invoice.issued"
	TopicInvoiceFailed    = "invoice.failed"
)

var _ = register(
	spec("billing", Outbox, InvoiceRequested{}),
	spec("billing", Outbox, InvoiceIssued{}),
	spec("billing", AfterCommit, InvoiceFailed{}),
)

// InvoiceRequested: the provider flow starts (invoice_automation=provider).
type InvoiceRequested struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
}

// Topic implements Payload.
func (InvoiceRequested) Topic() string { return TopicInvoiceRequested }

// InvoiceIssued: the PDF e-mail is sent to the buyer side.
type InvoiceIssued struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
}

// Topic implements Payload.
func (InvoiceIssued) Topic() string { return TopicInvoiceIssued }

// InvoiceFailed: finance is notified.
type InvoiceFailed struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
	Attempts  int       `json:"attempts"`
}

// Topic implements Payload.
func (InvoiceFailed) Topic() string { return TopicInvoiceFailed }
