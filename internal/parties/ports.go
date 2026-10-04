package parties

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
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
// (purpose party_document, 04 §1.14).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the file to its document row in the caller's transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
}

// Orgs is what parties needs from orgs (L5): the org of an org-surface
// request (existence → 404, kind → buyer parties belong to companies,
// status → writes of a suspended org are 403). It is orgs.OrgDirectory.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
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
