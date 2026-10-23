// Package privacy holds the KVKK provider contracts (docs/design/01 §3.3).
//
// Every module that stores personal data implements Provider (data subject
// access/portability and erasure) and, when it has retention periods,
// RetentionSweeper. cmd/koro registers them on the container lists that the
// kvkk module reads; kvkk never imports the modules. The package is a leaf:
// it imports only the standard library.
package privacy

import (
	"context"
	"time"
	"uuid"
)

// Provider exports and erases one module's personal data of a user.
type Provider interface {
	// Name is the owning module name ("events", "feed", ...).
	Name() string
	// Export returns the user's data for access/portability requests.
	Export(ctx context.Context, userID uuid.UUID) (ExportSection, error)
	// Erase deletes or anonymises the user's data on account deletion.
	// It must be idempotent.
	Erase(ctx context.Context, userID uuid.UUID) (EraseResult, error)
}

// RetentionSweeper deletes or anonymises data whose retention period ended.
type RetentionSweeper interface {
	// PolicyCodes lists the retention_policies.code values the sweeper owns.
	PolicyCodes() []string
	// Sweep applies policy p as of now. It must be idempotent and work in
	// batches (at most 1000 rows per transaction).
	Sweep(ctx context.Context, p RetentionPolicy, now time.Time) (SweepResult, error)
}

// Retention methods (retention_policies.method).
const (
	MethodHardDelete  = "hard_delete"
	MethodAnonymize   = "anonymize"
	MethodNullColumns = "null_columns"
	MethodFileDelete  = "file_delete"
)

// RetentionPolicy mirrors one retention_policies row (02 §24).
type RetentionPolicy struct {
	Code          string
	OwnerModule   string
	Target        string // table name or "files:<purpose>"
	Retention     time.Duration
	Anchor        string // created_at, ended_at, closed_at, ...
	Method        string // Method* constants
	LegalApproved bool   // false → dry run only (count + log, no deletion)
	Enabled       bool
}

// ExportSection is one module's part of a data export.
type ExportSection struct {
	Module string
	Items  []map[string]any
}

// EraseResult reports what Erase did.
type EraseResult struct {
	Module         string
	Deleted        int
	Anonymized     int
	Retained       int
	RetainedReason string // legal basis for retained rows (e.g. financial records, 10 years)
}

// SweepResult reports what Sweep did.
type SweepResult struct {
	PolicyCode string
	Table      string
	Method     string
	Count      int
}

// Subject identifies the data subject and the KVKK request a Provider call
// serves. kvkk stores it in the context (WithSubject) so providers can
// reference the request in audit and destruction logs while keeping the
// 01 §3.3 signatures (userID only).
type Subject struct {
	UserID uuid.UUID // the data subject (ilgili kişi)
	DSRID  uuid.UUID // data_subject_requests.id; uuid.Nil for system-initiated runs
}

type subjectKey struct{}

// WithSubject returns ctx carrying s.
func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, s)
}

// SubjectFrom returns the Subject stored in ctx, if any.
func SubjectFrom(ctx context.Context) (Subject, bool) {
	s, ok := ctx.Value(subjectKey{}).(Subject)
	return s, ok
}

// Destruction is one destruction record row (02 §16.3) of a destruction a
// module runs outside kvkk.retention_sweep (T-318: events.purge_exports
// deletes an export file at its expiry). The sweepers return a SweepResult
// and kvkk records their run; a module job that destroys on its own clock
// records each destruction through a DestructionRecorder instead, so every
// destruction of personal data leaves its row in the same place.
//
// It carries no personal data: counts, opaque identifiers (a file id) and
// the job name only.
type Destruction struct {
	PolicyCode   string    // retention_policies.code; its owner_module must be Module
	RunID        uuid.UUID // the rows of one job run share it
	Module       string    // the destroying module ("events")
	Target       string    // empty → the policy's target ("files:event_export")
	Method       string    // Method* constants
	Count        int       // records destroyed (≥ 0)
	PeriodCutoff time.Time // the anchor values at or before it were destroyed
	StartedAt    time.Time
	FinishedAt   time.Time
	Details      map[string]any // no personal data (job, file_id, ...)
}

// DestructionRecorder appends destruction record rows. kvkk implements it
// (it owns destruction_logs); cmd/koro hands it to the modules late bound
// (hooks.destruction), so no module imports kvkk.
type DestructionRecorder interface {
	// Record appends one row in the caller's transaction (ctx), so the row
	// commits with the destruction or not at all.
	Record(ctx context.Context, d Destruction) error
}
