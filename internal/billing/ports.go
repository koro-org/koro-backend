package billing

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
)

// Ports of billing (01 §4.21 "Bağımlılık: → orders, parties, files,
// notify, audit"; BE-042 adds catalog for the invoice terms, orgs for the
// org surface and the notices, identity for the re-authentication). Each
// is satisfied by the provider's cross-module contract; the composition
// root binds them. Every read joins the caller's transaction.

// Orders is what the invoice plan reads from orders (L10). It is
// orders.Orders.
type Orders interface {
	// ForBilling returns an order with all its lines (any status).
	ForBilling(ctx context.Context, orderID uuid.UUID) (orders.Order, []orders.Line, error)
	// PayoutLines returns the lines a payout paid out.
	PayoutLines(ctx context.Context, payoutID uuid.UUID) ([]orders.Line, error)
}

// Offerings is what the plan reads from catalog (L9): the offering's
// invoice trigger and the service's invoice codes. It is catalog.Offerings.
type Offerings interface {
	InvoiceTerms(ctx context.Context, offeringID, policyID uuid.UUID) (catalog.InvoiceTerms, error)
}

// Parties reads the legal parties of an invoice (L7): the payee's, Koro's
// own (kind platform) and the recipient's contact address. It is
// parties.Parties.
type Parties interface {
	Get(ctx context.Context, partyID uuid.UUID) (parties.LegalParty, error)
	// Platform returns Koro's party or parties.ErrNotFound when `koro seed
	// bootstrap-commerce` has not run.
	Platform(ctx context.Context) (parties.LegalParty, error)
}

// Payees reads a payee (its legal party and seller org). It is
// parties.Payees.
type Payees interface {
	Get(ctx context.Context, payeeID uuid.UUID) (parties.Payee, error)
}

// Orgs is what billing needs from orgs (L5): the org of an org-surface
// request (unknown → 404) and, in the worker, the admins the notices go to.
// It is orgs.OrgDirectory.
type Orgs interface {
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
}

// Files is what billing needs from files (L2): the invoice PDF (purpose
// invoice_pdf).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the PDF to its invoice in the upload transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL (downloadInvoicePdf).
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// Read returns the PDF for the invoice e-mail (worker).
	Read(ctx context.Context, fileID uuid.UUID, maxBytes int64) ([]byte, files.FileMeta, error)
	// States returns the states of the PDFs of a response (one read;
	// BE-154b).
	States(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]files.State, error)
}

// AuditLogger is audit's logger (L1): every invoice write is logged in its
// transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Reauthenticator verifies the re-authentication proof of
// billing.integration.manage (03 §6.2) after authz.Can and before any
// write. It is identity.Service (L4).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Sealer envelope-encrypts the integration tokens (platform/crypto,
// BE-055: the shared Envelope).
type Sealer interface {
	Seal(ctx context.Context, purpose string, plaintext []byte) ([]byte, error)
}

// Sender is notify's Sender (L3): the invoice notices are sent by the worker.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Staff lists the finance admins in effect (06 H-33): who uploads Koro's
// invoices and verifies every upload.
type Staff interface {
	FinanceAdmins(ctx context.Context) ([]uuid.UUID, error)
}
