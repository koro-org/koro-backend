package catalog

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/parties"
)

// AuditLogger is what catalog needs from audit (L1): every write is logged
// in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Orgs is what catalog needs from orgs (L5): the seller org of a request
// (existence → 404, kind → seller kind, status → writes of a suspended org
// are 403), the seller names of the DTOs and, in the worker, the admins of
// a seller org and the reps of a university. It is orgs.OrgDirectory.
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

// Directory is what catalog needs from directory (L6): a club's university
// (the university decision, T-21; authz U cells) and the orgs of a
// university (browseOfferings?university_id). It is directory.Service.
type Directory interface {
	// GetClub returns the catalog data of a club org or directory.ErrNotFound.
	GetClub(ctx context.Context, orgID uuid.UUID) (directory.Club, error)
	// GetUniversity returns a university or directory.ErrNotFound.
	GetUniversity(ctx context.Context, id uuid.UUID) (directory.University, error)
	// UniversityOrgs returns the orgs of a university (its own org and its
	// clubs); an unknown university has none.
	UniversityOrgs(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)
}

// Events is what catalog needs from events (L8): the event an offering is
// bound to (it must be the seller's and still open) and its title in the
// DTOs. It is events.Events.
type Events interface {
	// Get returns the summary of an event (any status) or events.ErrNotFound.
	Get(ctx context.Context, eventID uuid.UUID) (events.EventSummary, error)
}

// Payees is what catalog needs from parties (L7, 01 §4.12): the default
// payee of a seller and the payee rules of submission and approval
// (BE-037: an active payee with an approved bank account). It is
// parties.Payees.
type Payees interface {
	// Get returns a payee (any status) or parties.ErrNotFound.
	Get(ctx context.Context, payeeID uuid.UUID) (parties.Payee, error)
	// DefaultFor returns the default payee of a seller org that is active now.
	DefaultFor(ctx context.Context, sellerOrgID uuid.UUID) (parties.Payee, bool, error)
	// ApprovedAccount returns the approved bank account of a payee or 409
	// parties.ErrNoApprovedAccount.
	ApprovedAccount(ctx context.Context, payeeID uuid.UUID) (parties.BankAccount, error)
	// IsActive reports whether the payee may be paid now.
	IsActive(ctx context.Context, payeeID uuid.UUID) (bool, error)
}

// Reauthenticator verifies the re-authentication proof of
// catalog.policy.manage (03 §6.2, 06 H-17) after authz.Can and before any
// write. It is identity.Service (L4).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Sender is notify's Sender (L3): the worker sends the offering and policy
// notices.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Users resolves the e-mail address of a policy notice's recipient
// (identity.Users; worker path only).
type Users interface {
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}

// Staff lists the platform staff in effect (06 H-33) for the notices: the
// platform admins review offerings and, with the finance admins, receive
// every commerce policy change (06 H-17).
type Staff interface {
	PlatformAdmins(ctx context.Context) ([]uuid.UUID, error)
	FinanceAdmins(ctx context.Context) ([]uuid.UUID, error)
}

// BuyerHeld returns how many units of an offering the buyer company holds
// in its open orders (not cancelled or expired): the per_buyer_limit of
// Reserve. orders (BE-040, L10) provides it through the composition root
// and reads it in the caller's transaction, under the offering's row lock.
// nil → 0 (without orders nothing was ever reserved).
type BuyerHeld func(ctx context.Context, offeringID, buyerOrgID uuid.UUID) (int, error)
