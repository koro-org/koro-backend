package parties

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
)

// AuditLogger is what parties needs from audit (L1): every write and every
// document download is logged in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Files is what parties needs from files (L2) for the party documents
// (purpose party_document) and the bank account evidence (purpose
// bank_account_evidence; 04 §1.14).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the file to its document or bank account row in the
	// caller's transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// States returns the states of the documents' and the accounts'
	// evidence files of a response (one read; BE-154b).
	States(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]files.State, error)
	// Evidence returns the states and the uploaders of the accounts'
	// evidence files (one read; finance's view hides the eyes from the
	// uploader, BE-159).
	Evidence(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]files.Evidence, error)
}

// Orgs is what parties needs from orgs (L5): the org of an org-surface
// request (existence → 404, kind → buyer parties belong to companies,
// payees to clubs and universities, status → writes of a suspended org are
// 403) and, in the worker, the admins of a payee's org (the security
// notices of an IBAN change). It is orgs.OrgDirectory.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// ActiveMemberIDs returns the user IDs of an org's active members.
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	// HasRole reports whether userID holds one of roles in orgID (active).
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
}

// Sealer envelope-encrypts the national ID of an individual party
// (02 §0.4 `*_enc`; platform/crypto). It is the shared crypto.Envelope.
// parties never opens the value: no response carries a TCKN (03 k21); the
// crypto.rewrap job re-seals it under a new key.
type Sealer interface {
	Seal(ctx context.Context, purpose string, plaintext []byte) ([]byte, error)
}

// Indexer computes the blind index of a national ID (02 §0.4 `*_bidx`):
// the uniqueness of legal_parties_tckn_uq without the clear value. It is
// crypto.Indexer.
type Indexer interface {
	BlindIndex(purpose string, v []byte) []byte
}

// Reauthenticator verifies the re-authentication proof of the R
// operations (03 §6.2: parties.bank_account.submit, approve, revoke) after
// authz.Can and before any business write. It is identity.Service (L4).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Sender is notify's Sender (L3): the security notices of the bank account
// flow (01 §6.2) are sent by the worker.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Users resolves the e-mail address of a notice's recipient (identity.Users;
// worker path only).
type Users interface {
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}
