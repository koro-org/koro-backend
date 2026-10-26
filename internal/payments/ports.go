package payments

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
)

// AuditLogger is what payments needs from audit (L1): every write is logged
// in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Orders is what payments needs from orders (L10, 01 §4.18): the order a
// transfer pays and, in the capture transaction (01 §6.3 "Havale
// eşleştirme"), the money applied to it. It is orders.Orders; every call
// joins the caller's transaction.
type Orders interface {
	// ForPayment returns what payments needs to match money to an order
	// (orders.ErrNotFound when there is none).
	ForPayment(ctx context.Context, orderID uuid.UUID) (orders.PayableOrder, error)
	// FindByReference finds the order of a reference code however it was
	// typed; found is false for a malformed or unknown code.
	FindByReference(ctx context.Context, ref string) (orders.PayableOrder, bool, error)
	// ApplyPayment adds captured money to the order in the capture
	// transaction (T-14: partial payments accumulate; a full payment pays
	// the order, activates its rights and returns its lines).
	//
	// sync-write: orders.Orders.ApplyPayment
	ApplyPayment(ctx context.Context, orderID, paymentID uuid.UUID, amount int64) (orders.PaymentEffect, error)
	// ConfirmDirectPayment records direct evidence once and settles when the
	// fee is also complete. Both steps share the order lock and transaction.
	//
	// sync-write: orders.Orders.ConfirmDirectPayment
	ConfirmDirectPayment(ctx context.Context, orderID, paymentID uuid.UUID) (orders.PaymentEffect, error)
	// GetLine returns an order line (the seller of a payment notice, the
	// shares of a refunded line).
	GetLine(ctx context.Context, lineID uuid.UUID) (orders.Line, error)
	// ApplyRefund records a completed refund of the order (a line of it when
	// lineID is set) in the completion transaction (BE-043): the order's
	// and the line's refunded amounts grow (422 orders.refund_exceeds_paid
	// beyond what was paid or what Koro holds of the line), a paid order
	// becomes partially_refunded or refunded, a fully refunded line loses
	// its rights.
	//
	// sync-write: orders.Orders.ApplyRefund
	ApplyRefund(ctx context.Context, orderID uuid.UUID, lineID *uuid.UUID, amount int64) error
	// RefundsDue lists the money that waits for a refund (a cancelled line
	// of a paid order, an order cancelled or expired with money paid),
	// oldest first: finance opens the refunds from it.
	RefundsDue(ctx context.Context, limit int) ([]orders.RefundDue, error)
	// ForBilling returns an order with all its lines (any status;
	// orders.ErrNotFound): the lines a buyer may ask a refund of (BE-132).
	ForBilling(ctx context.Context, orderID uuid.UUID) (orders.Order, []orders.Line, error)
}

// Poster is the ledger's Poster (L11, 01 §4.19): the capture posts
// payment_received and, once the order is paid, order_allocated in the
// capture transaction (02 §21.3 model A templates).
type Poster interface {
	// Post writes a balanced transaction in the transaction carried by ctx;
	// the same business event with the same entries is a replay.
	//
	// sync-write: ledger.Poster.Post
	Post(ctx context.Context, t ledger.Txn) (uuid.UUID, error)
}

// LedgerLines is what a line refund reads from the ledger (L11; BE-043,
// 06 H-59, BE-067). It is ledger.Module; every read joins the caller's
// transaction.
type LedgerLines interface {
	// LineReceivable is the net outstanding clawback, including collections.
	LineReceivable(ctx context.Context, lineID uuid.UUID) (int64, error)
	// LockPayees serialises refunds with payout creation before order/line row locks.
	// sync-write: ledger.LedgerLines.LockPayees
	LockPayees(ctx context.Context, payeeIDs []uuid.UUID) error
	// LinesInPayouts returns which of the lines are items of a live payout
	// (pending_approval, approved or paid), with the payout's ID.
	LinesInPayouts(ctx context.Context, lineIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
	// RefundedShares returns what the completed refunds took back from the
	// line so far, per share (seller, Koro's sale, commission, VAT).
	RefundedShares(ctx context.Context, lineID uuid.UUID) (ledger.LineShares, error)
	// LineAllocation returns what the ledger allocated of the line: its
	// payable amount, or 0 for Koro's own line paid before its template
	// (BE-067: the refund recognises it first).
	LineAllocation(ctx context.Context, lineID uuid.UUID) (int64, error)
}

// Parties is what payments needs of the legal parties (parties L7): Koro's
// own party, the owner of every collection account (02 §20.1).
type Parties interface {
	// Platform returns Koro's own party (kind platform) or parties.ErrNotFound.
	Platform(ctx context.Context) (parties.LegalParty, error)
}

// Files is what payments needs from files (L2): the transfer receipt of a
// recorded transfer and of a direct payment (purpose transfer_receipt) and
// the EFT receipt of a return or of a refund (purpose refund_receipt).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// PresignGet returns a short-lived download URL of a clean file
	// (files.ErrNotClean otherwise): a transfer's receipt (BE-129).
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// Attach binds the file to its transfer or payment row in the caller's
	// transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
}

// Reauthenticator verifies the re-authentication proof of the R operations
// (03 §6.2: payments.collection_account.create/approve/retire,
// payments.transfer.mark_returned, payments.refund.approve) after authz.Can
// and before any business write. It is identity.Service (L4).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Orgs is what payments needs from orgs (L5): the org of a refund request
// surface and the names of the buyer and the seller (BE-132), and for the
// notices the admins of the buying company and of the selling orgs. It is
// orgs.OrgDirectory.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// GetMany returns the summaries of the ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
	// ActiveMemberIDs returns the user IDs of an org's active members.
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	// HasRole reports whether userID holds one of roles in orgID (active).
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
}

// Sender is notify's Sender (L3): the worker sends the payments notices.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Users resolves the e-mail address of a security notice's recipient
// (identity.Users; worker path only).
type Users interface {
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}

// Staff lists the platform roles in effect (06 H-33): the security notices
// of a collection account go to every platform and finance admin, the
// unmatched transfer digest to every finance admin.
type Staff interface {
	FinanceAdmins(ctx context.Context) ([]uuid.UUID, error)
	PlatformAdmins(ctx context.Context) ([]uuid.UUID, error)
}
