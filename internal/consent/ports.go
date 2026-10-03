package consent

import (
	"context"
	"uuid"

	"koro/internal/audit"
)

// AuditLogger is what consent needs from audit (L1): every write and the
// admin proof reads are logged in the business transaction (03 §6.5).
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// ReauthProof is the "reauth" body member of an R operation (03 §6.2,
// 04 §1.11): a password (account without MFA) or a TOTP code (MFA account).
// It is never logged or stored; String hides it.
type ReauthProof struct {
	Password string
	TOTPCode string
}

// String implements fmt.Stringer without revealing the proof.
func (ReauthProof) String() string { return "[REDACTED]" }

// GoString keeps %#v from printing the proof.
func (ReauthProof) GoString() string { return "consent.ReauthProof([REDACTED])" }

// Reauthenticator verifies a re-authentication proof (03 §6.2:
// identity.Reauth.Verify, after authz.Can and before any business data).
// identity is L4, above consent: the composition root binds it late
// (cmd/koro/wire_consent.go). It returns the proof kind for the audit row
// ("password" | "totp") or identity's 403 identity.reauth_required /
// identity.reauth_failed; it also counts failed attempts (rate limit class
// reauth).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof ReauthProof) (kind string, err error)
}
