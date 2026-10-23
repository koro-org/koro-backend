package ledger

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
)

// Ports of the payout flow (BE-044; 01 §4.19 "Bağımlılık: → orders,
// parties, files, notify, audit"). Each is satisfied by the provider's
// cross-module contract; the composition root binds them.

// Orders is what the payouts need from orders (L10, 01 §4.18). It is
// orders.Orders; every call joins the caller's transaction.
type Orders interface {
	// PayableLines returns the payee's lines a payout may take as of asOf
	// (paid, fulfilled, payout hold elapsed, with a positive seller share after refunds, not paid out;
	// never a line of a direct-to-payee order, model C). Lines already in
	// a live payout are the ledger's to leave out.
	PayableLines(ctx context.Context, payeeID uuid.UUID, asOf time.Time) ([]orders.PayableLine, error)
	// MarkLinesPaidOut records the payout of the lines in the mark-paid
	// transaction (01 §6.3 "Payout ödendi").
	//
	// sync-write: orders.Orders.MarkLinesPaidOut
	MarkLinesPaidOut(ctx context.Context, lineIDs []uuid.UUID, payoutID uuid.UUID) error
	// GetLine returns an order line (the order of a payout item).
	GetLine(ctx context.Context, lineID uuid.UUID) (orders.Line, error)
	// ForPayment returns an order's reference code (the items' order_no).
	ForPayment(ctx context.Context, orderID uuid.UUID) (orders.PayableOrder, error)
	// CollectedLines returns a page of the lines of the orders Koro collected
	// in full (paid, not model C) after a line ID, in line ID order;
	// platformOnly keeps Koro's own platform-native lines. The allocation
	// check of ledger.integrity_check and the late recognition of Koro's own
	// lines read them (BE-067).
	CollectedLines(ctx context.Context, after uuid.UUID, platformOnly bool, limit int) ([]orders.CollectedLine, error)
	// ForAllocation rereads a collected line while locking its order and
	// then its line in the caller's transaction, before any ledger write.
	ForAllocation(ctx context.Context, lineID uuid.UUID) (orders.CollectedLine, error)
}

// Payees is what the payouts need from parties (L7, 01 §4.12): the payee
// of a request and its approved account, the only account money goes to
// (snapshotted at creation). It is parties.Payees.
type Payees interface {
	// Get returns a payee (any status) or parties.ErrNotFound.
	Get(ctx context.Context, payeeID uuid.UUID) (parties.Payee, error)
	// ApprovedAccount returns the payee's approved bank account (full IBAN)
	// or 409 parties.no_approved_account.
	ApprovedAccount(ctx context.Context, payeeID uuid.UUID) (parties.BankAccount, error)
	// IsActive reports whether the payee may be paid now.
	IsActive(ctx context.Context, payeeID uuid.UUID) (bool, error)
}

// Parties reads the legal party of a payee: the contact address of the
// payout.paid notice (worker). It is parties.Parties.
type Parties interface {
	Get(ctx context.Context, partyID uuid.UUID) (parties.LegalParty, error)
}

// Orgs is what the payouts need from orgs (L5): the org of an org-surface
// request (unknown → 404) and, in the worker, the admins of a seller org.
// It is orgs.OrgDirectory.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// ActiveMemberIDs returns the user IDs of an org's active members.
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	// HasRole reports whether userID holds one of roles in orgID (active).
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
}

// Files is what the payouts need from files (L2): the bank receipt (dekont,
// purpose payout_receipt) of a paid payout.
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the receipt to its payout in the mark-paid transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
}

// AuditLogger is what the payouts need from audit (L1): every payout write
// is logged in its transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Reauthenticator verifies the re-authentication proof of the payout
// approval (03 §6.2: ledger.payout.approve) after authz.Can and before any
// business write. It is identity.Service (L4).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Sender is notify's Sender (L3): the payout notices are sent by the worker.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Staff lists the finance and platform admins in effect (06 H-33): the
// payout notices of the four eyes and the security notice of a transfer
// dated before its approval (BE-092).
type Staff interface {
	FinanceAdmins(ctx context.Context) ([]uuid.UUID, error)
	PlatformAdmins(ctx context.Context) ([]uuid.UUID, error)
}

// Users resolves a staff member's address and name for the security notice
// of a transfer dated before its approval (identity.Users; worker path only,
// BE-092). An unknown or erased user is identity.ErrNotFound.
type Users interface {
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}
