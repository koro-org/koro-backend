// Package identity is the identity module (01 §4.6; BE-014): registration
// with the age gate, argon2id passwords, opaque server-side sessions (web
// cookie, mobile bearer), derived e-mail tokens (verification, password
// reset, e-mail change), stateless re-authentication, /me and the
// credential → principal step of every request's Actor.
//
// Security rules the package enforces (05 §14, ASVS 5.0 L2):
//
//   - Sessions are 32 random bytes (platform/token), sent once; only their
//     SHA-256 is stored. A session is bound to its transport: a web session
//     (cookie) is never accepted as a bearer token and vice versa
//     (03 §2.2-1). Idle and absolute windows are enforced on the server;
//     MFA roles get 30 min / 12 h whatever the stored window says.
//   - Passwords: argon2id with a pepper (platform/passwd), 15–128
//     characters without MFA (8 with MFA), common and breached passwords
//     refused (embedded list, no network call), account lockout with
//     exponential back-off per account, never per IP (05 §14.1).
//   - No account enumeration: login does one argon2id verification for
//     every request (a dummy one for unknown or locked accounts); register,
//     resend and reset answer 202 whether the address exists or not, and
//     register hashes the password in both cases (ASVS 6.3.8).
//   - E-mail tokens are derived (HMAC, crypto.Deriver) from the token row's
//     ID and never stored or put into an event: the worker re-derives them.
//   - Passwords, tokens and addresses are never logged.
//   - A platform role is in effect only while its assignment is active or
//     revoke_pending (06 H-33): a pending request grants nothing.
package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Authenticator turns a credential into a principal (01 §4.6). Only the
// ActorResolver (cmd/koro/actor.go) calls it.
type Authenticator interface {
	// Authenticate returns the principal of a live session whose client
	// kind matches the transport, of an active user, and slides the idle
	// window (at most once a minute). Every rejection is
	// ErrUnauthenticated (03 §2.2-2); other errors are server failures.
	Authenticate(ctx context.Context, cred httpx.Credential) (Principal, error)
}

// Principal is an authenticated session (01 §4.6).
type Principal struct {
	UserID        uuid.UUID
	SessionID     uuid.UUID
	ClientKind    ClientKind
	MFAVerified   bool
	PlatformRoles []authz.PlatformRole // in effect only (06 H-33)
	Memberships   []authz.Membership   // hooks.memberships (orgs); none until BE-019
	BirthDate     *civil.Date          // IsMinor is computed per request by the resolver
}

// UserSummary is a user as other modules see it: no e-mail (01 §4.6).
type UserSummary struct {
	ID        uuid.UUID
	Status    Status
	FirstName string
	LastName  string
	Locale    string
	IsMinor   bool
}

// UserContact is the address of a user, for the worker/notification path
// only (01 §5.3).
type UserContact struct {
	ID        uuid.UUID
	Email     string
	FirstName string
	LastName  string
	Locale    string
}

// Users is the user directory of other modules (01 §4.6).
type Users interface {
	// Get returns a user or ErrNotFound (tombstones included: status erased).
	Get(ctx context.Context, id uuid.UUID) (UserSummary, error)
	// GetMany returns the users of ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]UserSummary, error)
	// Contact returns the address of a live user; ErrNotFound for unknown
	// or erased users. Worker/notification path only.
	Contact(ctx context.Context, id uuid.UUID) (UserContact, error)
	// FindByEmail returns the account of an address (invitations).
	FindByEmail(ctx context.Context, email string) (uuid.UUID, bool, error)
}

// Sessions is what other modules may do to sessions.
type Sessions interface {
	// RevokeAll closes every open session of userID with reason, in the
	// transaction carried by ctx (03 §2.2-6: role_change; orgs and the
	// platform role flow call it). It returns how many it closed.
	RevokeAll(ctx context.Context, userID uuid.UUID, reason RevokeReason) (int, error)
}

// ReauthProof is the "reauth" member of an R operation (03 §6.2, 04 §1.11).
// It is never logged or stored; String hides it.
type ReauthProof struct {
	Password string
	TOTPCode string
}

// String implements fmt.Stringer without revealing the proof.
func (ReauthProof) String() string { return "[REDACTED]" }

// GoString keeps %#v from printing the proof.
func (ReauthProof) GoString() string { return "identity.ReauthProof([REDACTED])" }

// Reauthenticator verifies a re-authentication proof (03 §6.2): after
// authz.Can, before any business data. Stateless: there is no "valid for 5
// minutes" window (T-51).
type Reauthenticator interface {
	// Verify returns the proof kind ("password" | "totp") or
	// ErrReauthRequired (no proof), ErrReauthFailed (wrong proof),
	// problem.ErrRateLimited (too many failures: account lockout or the
	// reauth class) or ErrMFAUnavailable. Failures count towards the
	// account lockout.
	Verify(ctx context.Context, userID uuid.UUID, proof ReauthProof) (kind string, err error)
}

// EduVerification is an active student verification (01 §4.6; BE-018).
type EduVerification struct {
	UniversityID uuid.UUID
	VerifiedAt   time.Time
	ExpiresAt    time.Time
	Method       string
}

// StudentVerification answers whether a user is a verified student
// (01 §4.6). This ticket ships the stub (nobody is verified); BE-018 fills
// it in.
type StudentVerification interface {
	Active(ctx context.Context, userID uuid.UUID) (EduVerification, bool, error)
	ActiveMany(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]EduVerification, error)
}

// Service is the identity service: the cross-module contracts plus the
// HTTP use cases (04 §2.1). Every method that takes an actor calls
// authz.Can first (05 §5.3); R operations then verify the proof (03 §6.2).
type Service interface {
	Authenticator
	Users
	Sessions
	Reauthenticator

	// Register is registerAccount: always nil for a well-formed request,
	// whether the address is new or taken (202, no enumeration).
	Register(ctx context.Context, a authz.Actor, in Registration) error
	// CreateSession is createSession (login).
	CreateSession(ctx context.Context, a authz.Actor, in Login) (LoginResult, error)
	// DeleteCurrentSession is deleteCurrentSession (logout).
	DeleteCurrentSession(ctx context.Context, a authz.Actor) error
	// ListMySessions is listMySessions: the caller's live sessions.
	ListMySessions(ctx context.Context, a authz.Actor, q SessionQuery) (SessionPage, error)
	// RevokeMySession is revokeMySession; another user's session is 404.
	RevokeMySession(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// ConfirmEmail is confirmEmail: pending_email → active.
	ConfirmEmail(ctx context.Context, a authz.Actor, token string) error
	// ResendVerification is resendEmailVerification (202, no enumeration).
	ResendVerification(ctx context.Context, a authz.Actor, email string) error
	// RequestPasswordReset is requestPasswordReset (202, no enumeration).
	RequestPasswordReset(ctx context.Context, a authz.Actor, email string) error
	// ConfirmPasswordReset is confirmPasswordReset: every session closes.
	ConfirmPasswordReset(ctx context.Context, a authz.Actor, token, newPassword string) error
	// ChangePassword is changeMyPassword (R): the other sessions close.
	ChangePassword(ctx context.Context, a authz.Actor, in PasswordChange) error
	// RequestEmailChange is requestEmailChange (R): a token to the new
	// address, a notice to the old one.
	RequestEmailChange(ctx context.Context, a authz.Actor, newEmail string, proof ReauthProof) error
	// ConfirmEmailChange is confirmEmailChange.
	ConfirmEmailChange(ctx context.Context, a authz.Actor, token string) error
	// Me is getMe.
	Me(ctx context.Context, a authz.Actor) (Me, error)
	// UpdateMe is updateMe (names, locale).
	UpdateMe(ctx context.Context, a authz.Actor, p MePatch) (Me, error)
}

// PasswordHasher is platform/passwd.Hasher (an interface so tests can count
// the argon2id work of each path).
type PasswordHasher interface {
	Hash(ctx context.Context, pw string) (string, error)
	Verify(ctx context.Context, pw, encoded string) (ok, needsRehash bool, err error)
	VerifyDummy(ctx context.Context, pw string) error
}

// Deps are the dependencies of the identity module. Everything but Hasher,
// Memberships, MFA and TOTP is required (New returns an error otherwise).
type Deps struct {
	Store   Store
	Tx      db.TxManager
	Outbox  outbox.Writer  // identity.email_token_issued (O), user.active_day (A)
	Queue   queue.Enqueuer // identity.send_security_notice (A, no topic)
	Audit   AuditLogger
	Clock   clock.Clock
	Log     *slog.Logger
	Limiter Limiter         // auth_email, auth_password_reset and reauth in the service (06 H-6)
	Deriver *crypto.Deriver // e-mail tokens (CRYPTO_TOKEN_KEYS) and the e-mail digest of rate limit keys
	Consent ConsentRecorder // registration consents (same transaction)
	Notices NoticeLookup    // the privacy notice shown at registration
	Mailer  Mailer          // worker: verification, reset, change and security e-mails

	// LinkBaseURL is IDENTITY_LINK_BASE_URL (web application origin).
	LinkBaseURL string

	// Hasher hashes and verifies passwords. nil (worker) → every password
	// operation answers 503: only the API and the seed command load the
	// pepper.
	Hasher PasswordHasher
	// Memberships returns org memberships (hooks.memberships, late bound).
	// nil → none (03 §2.1: personal and public actions only).
	Memberships MembershipsFunc
	// MFA opens the login challenge of an MFA account; TOTP verifies a code
	// on re-authentication (BE-016). nil → MFA accounts get 503
	// identity.mfa_unavailable (fail closed).
	MFA  MFAChallenger
	TOTP TOTPVerifier
}

func (d *Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil},
		{"Audit", d.Audit != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil}, {"Limiter", d.Limiter != nil},
		{"Deriver", d.Deriver != nil}, {"Consent", d.Consent != nil}, {"Notices", d.Notices != nil},
		{"Mailer", d.Mailer != nil}, {"LinkBaseURL", d.LinkBaseURL != ""},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("identity.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what identity hands to the composition root.
type Module struct {
	Service Service
	// Students is the StudentVerification stub (BE-018 replaces it).
	Students StudentVerification
	// Privacy is the KVKK provider: the account becomes a tombstone and
	// its sessions and tokens are deleted (kvkk calls it last).
	Privacy privacy.Provider

	svc *service
}

// PlatformAdmins returns the users holding platform_admin in effect
// (06 H-33: active or revoke_pending; a pending request does not count).
// notify's admin alerts (06 H-40) read it through cmd/koro.
func (m Module) PlatformAdmins(ctx context.Context) ([]uuid.UUID, error) {
	return m.svc.d.Store.UsersWithRole(ctx, authz.PlatformAdmin)
}

// New builds the identity module.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Students: noStudentVerification{}, Privacy: privacyProvider{s: s}, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds), so
// the in-memory store keeps exactly what PostgreSQL stores.
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }

// noStudentVerification is the StudentVerification stub of this ticket:
// nobody is a verified student until BE-018 (Actor.StudentStatus none).
type noStudentVerification struct{}

func (noStudentVerification) Active(context.Context, uuid.UUID) (EduVerification, bool, error) {
	return EduVerification{}, false, nil
}

func (noStudentVerification) ActiveMany(context.Context, []uuid.UUID) (map[uuid.UUID]EduVerification, error) {
	return map[uuid.UUID]EduVerification{}, nil
}
