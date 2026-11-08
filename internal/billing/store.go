package billing

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence of billing (05 §2): invoices, their lines and
// the integration credentials. The GORM store (store_gorm.go) and the
// in-memory store (store_mem.go) pass the same contract test. Writes join
// the transaction carried by ctx.
//
// The database is the last line (000116): the four-eyes CHECK of the
// verification (06 H-19), one CHECK per state (06 H-37), the unique
// idempotency key of a planned document, the issuer's unique invoice
// number and the frozen document once issued.
type Store interface {
	// InsertInvoice writes a planned invoice and its lines unless an
	// invoice with its idempotency key exists (a repeated event): then
	// nothing is written and the existing row is returned, created false.
	InsertInvoice(ctx context.Context, inv Invoice, lines []Line) (stored Invoice, created bool, err error)
	// GetInvoice returns an invoice or ErrNotFound; lock takes its row lock
	// until the running transaction ends.
	GetInvoice(ctx context.Context, id uuid.UUID, lock bool) (Invoice, error)
	// InvoiceLines returns the lines of an invoice by position.
	InvoiceLines(ctx context.Context, invoiceID uuid.UUID) ([]Line, error)
	// UpdateInvoice writes the running columns of next (status, document
	// type, number, ETTN, issue date, PDF, uploader, verifier, issued_at,
	// last_error, attempts and the provider references) when the stored row
	// is still in state from; ErrInvalidState otherwise. The planned
	// columns (the reserved series and number among them) are never written
	// (06 H-56).
	UpdateInvoice(ctx context.Context, next Invoice, from Status) (Invoice, error)
	// ReserveInvoiceNo returns the next number of the provider flow's
	// series: one above the highest reserved_no of the series. It holds a
	// transaction-scoped lock until the running transaction ends, so the
	// caller inserts the invoice carrying the number in that transaction
	// and concurrent plans never take the same number (BE-053).
	ReserveInvoiceNo(ctx context.Context, series string) (int, error)
	// ProviderContact returns the contact ID of a legal party at a provider
	// (provider_contact_map) or ErrNotFound.
	ProviderContact(ctx context.Context, partyID uuid.UUID, provider string) (string, error)
	// PutProviderContact inserts or replaces the contact ID of a legal party
	// at a provider.
	PutProviderContact(ctx context.Context, partyID uuid.UUID, provider, contactID string) error
	// ListInvoices returns up to limit invoices matching f, newest first
	// (created_at, then ID), strictly after the cursor.
	ListInvoices(ctx context.Context, f Filter, after *Cursor, limit int) ([]Invoice, error)
	// InvoicesOfOrderLine returns the invoices with a line of the order
	// line, oldest first.
	InvoicesOfOrderLine(ctx context.Context, orderLineID uuid.UUID) ([]Invoice, error)
	// InvoicesBy returns the invoices a user uploaded or verified (KVKK
	// export; financial records).
	InvoicesBy(ctx context.Context, userID uuid.UUID) ([]Invoice, error)

	// Credentials returns every integration_credentials row by provider.
	Credentials(ctx context.Context) ([]Credential, error)
	// Credential returns the integration_credentials row of a provider or
	// ErrNotFound; lock takes its row lock until the running transaction
	// ends (the token rotation's single refresher, 02 §22.5).
	Credential(ctx context.Context, provider string, lock bool) (Credential, error)
	// PutCredential inserts or replaces the row of c.Provider.
	PutCredential(ctx context.Context, c Credential) (Credential, error)
}

// Direction narrows an org's invoices: the ones it issued or received.
type Direction string

// Directions of listOrgInvoices.
const (
	DirectionIssued   Direction = "issued"
	DirectionReceived Direction = "received"
)

// Filter narrows ListInvoices.
type Filter struct {
	OrgID     *uuid.UUID // the org surface: issuer or recipient org (Direction narrows)
	Direction Direction  // "" = both sides
	Kind      Kind       // "" = any
	Status    Status     // "" = any
	OrderID   *uuid.UUID
	PayoutID  *uuid.UUID
}

// Cursor is a keyset position (created_at, id) of an invoice list.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}
