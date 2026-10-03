package identity

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
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

// MFAChallenger opens the 5-minute, single-use login challenge of an MFA
// account (Redis; BE-016 implements it and completeMfaLogin).
type MFAChallenger interface {
	Begin(ctx context.Context, userID uuid.UUID, client ClientKind, deviceLabel string) (MFAChallenge, error)
}

// TOTPVerifier checks a TOTP code of an MFA account during
// re-authentication (BE-016; replay guard included).
type TOTPVerifier interface {
	VerifyTOTP(ctx context.Context, userID uuid.UUID, code string) (bool, error)
}

// MembershipsFunc returns the active org memberships of a user (orgs,
// through cmd/koro's hooks.memberships; 03 §2.1). identity needs them for
// the session policy (03 §6.4: MFA roles get 30 min / 12 h sessions) and
// hands them to the Actor; it never imports orgs (01 §5.3).
type MembershipsFunc func(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error)
