package kvkk

import (
	"context"
	"encoding/json"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/notify"
)

// AuditLogger is what kvkk needs from audit (L1): every step of a request
// and every export download is logged in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
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
func (ReauthProof) GoString() string { return "kvkk.ReauthProof([REDACTED])" }

// Reauthenticator verifies a re-authentication proof (identity.Reauth,
// 03 §6.2): after authz.Can, before any business data. It returns the
// proof kind for the audit row ("password" | "totp") or identity's 403
// identity.reauth_required / identity.reauth_failed, and counts failures
// (429 once exhausted). Any other error is no verdict on the proof: the
// request context ended before the check (overload, BE-070), or a
// dependency failed; it is returned unchanged and never audited as denied
// (BE-072).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof ReauthProof) (kind string, err error)
}

// Contact is the address of a live user (identity.Users.Contact, worker
// path only, 01 §5.3).
type Contact struct {
	Email     string
	FirstName string
	Locale    string
}

// Users is what kvkk reads from identity (L4).
type Users interface {
	// Exists reports whether a user (tombstones included) exists.
	Exists(ctx context.Context, id uuid.UUID) (bool, error)
	// Contact returns the address of a live user; found=false for unknown
	// or erased users.
	Contact(ctx context.Context, id uuid.UUID) (c Contact, found bool, err error)
}

// Files is what kvkk needs from files (L2) for the export file.
type Files interface {
	// PutGenerated stores the export zip (outside a transaction).
	PutGenerated(ctx context.Context, in files.GeneratedFile) (uuid.UUID, error)
	// Attach binds the export to its request in the caller's transaction,
	// so orphan cleanup leaves it alone.
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// Get returns the file metadata (the creation time anchors the 7-day
	// retention).
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// PresignGet returns a short-lived download URL (≤ 5 minutes).
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// Delete removes an export file in the caller's transaction after kvkk
	// cleared its reference; the objects go after the commit (06 H-49,
	// T-193: the export file dies with its reference or not at all).
	// sync-write: files.Files.Delete
	Delete(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
}

// SessionRevoker closes every open session of a user (identity's
// Sessions.RevokeAll with reason erasure, bound in cmd/koro). kvkk calls it
// when an erasure is approved, after that transaction has committed, and
// again when the erasure run starts (06 H-49, T-195): the subject is signed
// out at once instead of only when identity anonymises the account. It is
// never called inside a kvkk transaction (RevokeAll is not on the 01 §6.3
// list of same-transaction writes). nil → the erasure run still deletes
// every session at its identity step.
type SessionRevoker func(ctx context.Context, userID uuid.UUID) (int, error)

// PlatformAdmins lists the users holding platform_admin in effect (06 H-33;
// identity, bound in cmd/koro): breach.recorded and the breach timer alert
// them in-app. nil → only the DPO mailbox is alerted.
type PlatformAdmins func(ctx context.Context) ([]uuid.UUID, error)

// Mailer sends e-mail in the worker (notify, L3).
type Mailer interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// TalentAuditor is the talent auditor report of a profiling_access request
// (hooks.talentAuditor; talent BE-047/BE-052). nil → the export says that
// no profiling report exists.
type TalentAuditor interface {
	// Available reports whether talent provides the report (the hook is
	// late bound by talent).
	Available() bool
	Report(ctx context.Context, requestedBy, userID uuid.UUID, reason string) (json.RawMessage, error)
}
