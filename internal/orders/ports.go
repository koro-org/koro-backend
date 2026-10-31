package orders

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/parties"
)

// AuditLogger is what orders needs from audit (L1): every write is logged
// in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Orgs is what orders needs from orgs (L5): the buying company of a request
// (existence → 404, status → k25), the seller names of the DTOs and, in the
// worker, the admins of an org. It is orgs.OrgDirectory.
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

// Offerings is what orders needs from catalog (L9, 01 §6.3 "Sipariş
// verme"): the offering on sale and the policy in effect, the stock
// reservation and its release. It is catalog.Offerings; every call joins
// the caller's transaction.
type Offerings interface {
	// ForCheckout returns the offering on sale (catalog.ErrNotFound,
	// ErrNotOnSale, ErrSoldOut).
	ForCheckout(ctx context.Context, offeringID uuid.UUID) (catalog.CheckoutOffering, error)
	// ResolvePolicy returns the commerce policy in effect at t
	// (catalog.ErrNoPolicy → 409 catalog.no_commerce_policy).
	ResolvePolicy(ctx context.Context, offeringID uuid.UUID, at time.Time) (catalog.CommercePolicy, error)
	// Reserve takes qty units for the buyer in the order's transaction.
	//
	// sync-write: catalog.Offerings.Reserve
	Reserve(ctx context.Context, offeringID uuid.UUID, qty int, buyerOrgID uuid.UUID) error
	// Release gives the units of an expired or cancelled order back in its
	// transaction.
	//
	// sync-write: catalog.Offerings.Release
	Release(ctx context.Context, offeringID uuid.UUID, qty int) error
}

// Parties is what orders needs of the legal parties (parties L7): the
// billed party of an order (03 k25) and its snapshot. It is parties.Parties.
type Parties interface {
	// Get returns a party (any kind and status) or parties.ErrNotFound.
	Get(ctx context.Context, partyID uuid.UUID) (parties.LegalParty, error)
}

// Payees is what orders needs of the payees (parties L7, 01 §6.3): the
// default payee of a seller and whether it may be paid. It is
// parties.Payees.
type Payees interface {
	// Get returns the frozen payee's current legal-party link.
	Get(ctx context.Context, payeeID uuid.UUID) (parties.Payee, error)
	// ApprovedAccount returns the current four-eyes approved bank account.
	ApprovedAccount(ctx context.Context, payeeID uuid.UUID) (parties.BankAccount, error)
	// DefaultFor returns the default payee of a seller org that is active now.
	DefaultFor(ctx context.Context, sellerOrgID uuid.UUID) (parties.Payee, bool, error)
	// IsActive reports whether the payee may be paid now.
	IsActive(ctx context.Context, payeeID uuid.UUID) (bool, error)
}

// Entitlements is what orders needs from entitlements (L2): the pending
// rights of a platform-native line, their activation once the order is
// paid and their revocation when an unpaid order ends or a line is
// refunded. It is entitlements.Entitlements; every call joins the
// caller's transaction.
type Entitlements interface {
	// Grant writes the pending right of an order line (01 §6.3).
	//
	// sync-write: entitlements.Entitlements.Grant
	Grant(ctx context.Context, in entitlements.GrantInput) (uuid.UUID, error)
	// ActivateBySource activates the rights of a paid line (01 §6.3
	// "Havale eşleştirme": orders.ApplyPayment → ActivateBySource).
	//
	// sync-write: entitlements.Entitlements.ActivateBySource
	ActivateBySource(ctx context.Context, t entitlements.SourceType, sourceID uuid.UUID) error
	// RevokeBySource revokes the rights of a line whose order expired or was
	// cancelled unpaid, or that was refunded (01 §7 orders.expire_unpaid).
	//
	// sync-write: entitlements.Entitlements.RevokeBySource
	RevokeBySource(ctx context.Context, t entitlements.SourceType, sourceID uuid.UUID, reason string) error
}

// Files is what orders needs from files (L2): the seller's delivery
// evidence of a line (purpose delivery_evidence; BE-093, T-292). It is
// files.Files.
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the evidence to its line in the marking transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file
	// (getOrderLineEvidence).
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// States returns the states of the evidence files of the lines shown
	// (BE-154).
	States(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]files.State, error)
}

// CollectionAccount is Koro's active default collection account (payments,
// BE-041) shown in the payment instructions. It is not personal data.
type CollectionAccount struct {
	ID         uuid.UUID
	IBAN       string
	HolderName string
	BankName   string
}

// CollectionAccounts returns Koro's active default collection account;
// found is false when none is active (hooks.collectionAccount; nil → 409
// payments.no_active_collection_account).
type CollectionAccounts func(ctx context.Context) (acct CollectionAccount, found bool, err error)

// Sender is notify's Sender (L3): the worker sends the order notices.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Users resolves the e-mail address of the buyer who placed an order
// (identity.Users; worker path only).
type Users interface {
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}

// Staff lists the finance admins in effect (06 H-33): they see every new
// order (01 §6.2 order.placed).
type Staff interface {
	FinanceAdmins(ctx context.Context) ([]uuid.UUID, error)
}
