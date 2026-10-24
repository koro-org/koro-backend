package audit

import "koro/internal/platform/problem"

// Errors of the audit module (05 §4.2).
var (
	// ErrInvalidEvent: an event breaks the audit_events contract (action
	// format, outcome, actor, reason length, metadata shape). A programmer
	// error: the HTTP layer answers 500 without details.
	ErrInvalidEvent = problem.New(problem.Internal, "audit.invalid_event", "Audit event is invalid")

	// ErrSecretMetadata: the metadata of an event carries a password, a
	// token, a key or special-category personal data (KVKK md. 6), by key
	// name or by value shape. Never stored; the cause names the key only.
	ErrSecretMetadata = problem.New(problem.Internal, "audit.secret_metadata", "Audit metadata must not contain secrets")

	// ErrInvalidRange: occurred_from/occurred_to or from_day/to_day are
	// reversed, in the future or span too long (422).
	ErrInvalidRange = problem.New(problem.Invalid, "audit.invalid_range", "Invalid time range")

	// ErrSignerNotConfigured: no daily root key is configured: signing in
	// the worker (AUDIT_ROOT_SIGNING_KEYS) or verifying in the API or worker
	// (AUDIT_ROOT_VERIFY_KEYS, BE-107).
	ErrSignerNotConfigured = problem.New(problem.Unavailable, "audit.signer_not_configured",
		"Audit root signing is not configured")

	// ErrArchiveNotConfigured: no separate root store is configured
	// (AUDIT_ROOT_ARCHIVE_DIR).
	ErrArchiveNotConfigured = problem.New(problem.Unavailable, "audit.archive_not_configured",
		"Audit root archive is not configured")

	// ErrArchiveConflict: the archive already holds a different object under
	// the key of a daily root (write-once violated: tampering or a bug).
	ErrArchiveConflict = problem.New(problem.Conflict, "audit.archive_conflict",
		"Audit root archive holds a different root")

	// ErrRootExists: a daily root of the day is already stored (the job is
	// idempotent and treats it as done).
	ErrRootExists = problem.New(problem.Conflict, "audit.root_exists", "Audit daily root already exists")
)
