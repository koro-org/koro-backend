package identity

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/platform/ratelimit"
)

// AuditLogger is what identity needs from audit (L1): every write, failed
// logins and failed re-authentications are logged in the business
// transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// ConsentRecorder records the consents given at registration in the
// registration transaction (01 §6.3 "identity kayıt → consent.Recorder.Record").
type ConsentRecorder interface {
	// Record appends one consent act in the transaction carried by ctx.
	// sync-write: consent.Recorder.Record
	Record(ctx context.Context, in consent.RecordInput) (uuid.UUID, error)
}

// NoticeInfo is what registration needs to know about the privacy notice
// the client showed (02 §4.2).
type NoticeInfo struct {
	ID         uuid.UUID
	Kind       consent.NoticeKind
	Purpose    consent.Purpose // consent texts only
	Audience   consent.Audience
	Version    string
	BodySHA256 []byte
	Published  bool // published and not retired: the current version of its key
}

// NoticeLookup finds a notice version (consent, L2). Adapted in
// cmd/koro/wire_identity.go from consent.Service.GetNotice.
type NoticeLookup interface {
	// NoticeByID returns the notice or ErrNotFound (drafts are not found).
	NoticeByID(ctx context.Context, id uuid.UUID) (NoticeInfo, error)
}

// Mailer sends e-mail in the worker (notify, L3; 01 §5.3: identity passes
// the address, notify never resolves recipients).
type Mailer interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Limiter is the rate limiter of the classes the middleware cannot key
// (04 §1.9: auth_email's e-mail digest, reauth's failed attempts; 06 H-6).
type Limiter interface {
	Allow(ctx context.Context, rule ratelimit.Rule, key string) (ratelimit.Result, error)
}

// MFAChallenge is the answer of a login that needs a second factor
// (03 §6.4-1: no half-authenticated session exists).
type MFAChallenge struct {
	ID        uuid.UUID
	ExpiresAt time.Time
}

// SecretBox seals and opens secrets at rest (crypto.Envelope).
type SecretBox interface {
	Seal(ctx context.Context, purpose string, plaintext []byte) ([]byte, error)
	Open(ctx context.Context, purpose string, sealed []byte) ([]byte, error)
}

// ChallengeStore keeps short-lived login challenges (Redis in production,
// 06 H-16; cmd/koro adapts go-redis, identity never imports it). Keys are
// digests of the challenge IDs, so the store never holds a usable ID.
type ChallengeStore interface {
	// Put stores value under key for ttl (> 0).
	Put(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Take returns the value of key and removes it atomically (GETDEL):
	// a challenge is used at most once. found=false when there is none or
	// it expired.
	Take(ctx context.Context, key string) (value []byte, found bool, err error)
}

// MembershipsFunc returns the active org memberships of a user (orgs,
// through cmd/koro's hooks.memberships; 03 §2.1). identity needs them for
// the session policy (03 §6.4: MFA roles get 30 min / 12 h sessions) and
// hands them to the Actor; it never imports orgs (01 §5.3).
type MembershipsFunc func(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error)

// Files is what identity needs from files (L2) for student documents
// (01 §4.6): the upload's metadata, attaching it to its verification and
// the short-lived download of the review.
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the document to its verification in the caller's
	// transaction (orphan cleanup leaves it alone).
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a download URL valid for ttl (≤ 5 minutes) of a
	// clean file; files.ErrNotClean otherwise.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
}

// FileRemover deletes a student document (files.Files.Delete) once identity
// has cleared its reference: erasure, and a document the virus scan
// rejected (01 §6.2 file.rejected). It is a function, not a port method:
// the pair identity → files.Files.Delete is not in the 01 §6.3 allow-list
// yet (T-221; kvkk has the same gap, BE-021/H-49). nil → the file
// stays and a warning is logged.
type FileRemover func(ctx context.Context, a authz.Actor, fileID uuid.UUID) error

// DomainResolver maps the domain of a .edu.tr address to a university
// (hooks.domainResolver: directory.Catalog.UniversityByEmailDomain, 01 §5.3;
// identity never imports directory). found=false: not a known university.
type DomainResolver func(ctx context.Context, domain string) (universityID uuid.UUID, found bool, err error)

// UniversityNames returns the display names of universities (directory).
// Optional: nil, or a missing ID, leaves the name out of the answers.
type UniversityNames func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
