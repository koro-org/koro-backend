package kvkk

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/privacy"
)

// The KVKK records of kvkk-2 (BE-049; 02 §16.2–16.5): retention policies,
// the destruction record, the breach register and the sub-processor
// inventory. None of them is org-owned.

// Period is a retention period as PostgreSQL keeps it (interval: years,
// months, days, hours). Calendar arithmetic, never a fixed duration: ten
// years end on the same calendar day, leap days included, so a record is
// never destroyed a day early.
type Period struct {
	Years, Months, Days, Hours int
}

// ErrPeriod is a malformed period.
var ErrPeriod = errors.New("kvkk: period must be an ISO 8601 duration of years, months, days and hours (P3Y, P12M, P30D, PT24H, P0D)")

// maxPeriodYears bounds a period (the seed's longest is the 100-year
// placeholder of dormant_account).
const maxPeriodYears = 100

// ParsePeriod reads the ISO 8601 subset P[nY][nM][nD][T[nH]] (at least one
// part; "P0D" is zero).
func ParsePeriod(s string) (Period, error) {
	rest, ok := strings.CutPrefix(s, "P")
	if !ok || rest == "" {
		return Period{}, ErrPeriod
	}
	var p Period
	datePart, timePart, hasT := strings.Cut(rest, "T")
	if hasT && timePart == "" {
		return Period{}, ErrPeriod
	}
	parts := 0
	read := func(src string, units string, dst []*int) error {
		order := 0
		for src != "" {
			i := strings.IndexFunc(src, func(r rune) bool { return r < '0' || r > '9' })
			if i <= 0 || i > 6 {
				return ErrPeriod
			}
			n, err := strconv.Atoi(src[:i])
			if err != nil {
				return ErrPeriod
			}
			u := strings.IndexByte(units, src[i])
			if u < order {
				return ErrPeriod // unknown unit, repeated or out of order
			}
			*dst[u] = n
			order, src, parts = u+1, src[i+1:], parts+1
		}
		return nil
	}
	if err := read(datePart, "YMD", []*int{&p.Years, &p.Months, &p.Days}); err != nil {
		return Period{}, err
	}
	if err := read(timePart, "H", []*int{&p.Hours}); err != nil {
		return Period{}, err
	}
	if parts == 0 {
		return Period{}, ErrPeriod
	}
	p = p.normalized()
	if p.Years > maxPeriodYears || p.Days > 366*maxPeriodYears || p.Hours > 24*366 {
		return Period{}, ErrPeriod
	}
	return p, nil
}

// normalized folds 12 months into a year, as PostgreSQL does.
func (p Period) normalized() Period {
	p.Years += p.Months / 12
	p.Months %= 12
	return p
}

// String is the canonical ISO 8601 form ("P3Y", "P1Y6M", "PT24H", "P0D").
func (p Period) String() string {
	var b strings.Builder
	b.WriteString("P")
	for _, x := range []struct {
		n int
		u string
	}{{p.Years, "Y"}, {p.Months, "M"}, {p.Days, "D"}} {
		if x.n != 0 {
			b.WriteString(strconv.Itoa(x.n) + x.u)
		}
	}
	if p.Hours != 0 {
		b.WriteString("T" + strconv.Itoa(p.Hours) + "H")
	}
	if b.Len() == 1 {
		return "P0D"
	}
	return b.String()
}

// Before returns t minus the period (the retention cut-off at t).
func (p Period) Before(t time.Time) time.Time {
	return t.AddDate(-p.Years, -p.Months, -p.Days).Add(-time.Duration(p.Hours) * time.Hour)
}

// Duration is the period's length ending at t (calendar exact at t).
func (p Period) Duration(t time.Time) time.Duration { return t.Sub(p.Before(t)) }

// Retention policy anchors and methods (000089 CHECKs; 000121 adds
// revoked_or_rejected, T-121).
var (
	policyMethods = []string{privacy.MethodHardDelete, privacy.MethodAnonymize, privacy.MethodNullColumns, privacy.MethodFileDelete}
)

// ExportPolicyCode is the policy of the data export files (T-83, 06 H-48:
// never longer than 7 days).
const ExportPolicyCode = "dsr_export_file"

// ErasurePolicyCode is the destruction record category of account erasures
// (02 §16.3: one destruction_logs row per module; no sweeper).
const ErasurePolicyCode = "dsr_erasure"

// Policy is one retention_policies row (02 §16.2).
type Policy struct {
	Code          string
	OwnerModule   string
	Target        string
	Period        Period
	Anchor        string
	Method        string
	LegalBasis    string
	LegalApproved bool
	ApprovedRef   string
	Enabled       bool
	DescriptionTR string
	UpdatedBy     *uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RetentionPolicy is the sweeper's view of p at now (01 §3.3).
func (p Policy) RetentionPolicy(now time.Time) privacy.RetentionPolicy {
	return privacy.RetentionPolicy{Code: p.Code, OwnerModule: p.OwnerModule, Target: p.Target,
		Retention: p.Period.Duration(now), Anchor: p.Anchor, Method: p.Method,
		LegalApproved: p.LegalApproved, Enabled: p.Enabled}
}

// DestructionLog is one destruction_logs row (02 §16.3): no personal data,
// counts and errors only.
type DestructionLog struct {
	ID           uuid.UUID
	PolicyCode   string
	RunID        uuid.UUID
	Module       string
	Target       string
	Method       string
	RecordCount  int
	PeriodCutoff time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	Details      map[string]any
	CreatedAt    time.Time
}

// DestructionFilter filters adminListDestructionLogs.
type DestructionFilter struct {
	PolicyCode string
	From, To   *time.Time // created_at ∈ [From, To)
}

// BreachStatus is breach_incidents.status.
type BreachStatus string

// Breach statuses, in the only order they may follow.
const (
	BreachOpen      BreachStatus = "open"
	BreachContained BreachStatus = "contained"
	BreachNotified  BreachStatus = "notified"
	BreachClosed    BreachStatus = "closed"
)

func (s BreachStatus) rank() int {
	return slices.Index([]BreachStatus{BreachOpen, BreachContained, BreachNotified, BreachClosed}, s)
}

// Valid reports whether s is a known status.
func (s BreachStatus) Valid() bool { return s.rank() >= 0 }

// AuthorityPeriod is the deadline of the notification to the Board (KVKK
// md. 12/5; Kurul kararı 2019/10: "en geç 72 saat").
const AuthorityPeriod = 72 * time.Hour

// ControllerPeriod is the processor's deadline to tell the data controller
// (Proje Sahibi; the data processing agreement): 24 hours after detection.
const ControllerPeriod = 24 * time.Hour

// Breach is one breach_incidents row (02 §16.4).
type Breach struct {
	ID                   uuid.UUID
	Title                string
	DetectedAt           time.Time
	AuthorityDeadlineAt  time.Time // DetectedAt + 72 hours
	ControllerNotifiedAt *time.Time
	AuthorityNotifiedAt  *time.Time
	DataCategories       []string
	AffectedEstimate     *int
	Description          string
	Status               BreachStatus
	OwnerUserID          uuid.UUID
	ClosedAt             *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// ControllerDeadlineAt is when the data controller must have been told.
func (b Breach) ControllerDeadlineAt() time.Time { return b.DetectedAt.Add(ControllerPeriod) }

func (b Breach) clone() Breach {
	c := b
	c.DataCategories = slices.Clone(b.DataCategories)
	for _, p := range []**time.Time{&c.ControllerNotifiedAt, &c.AuthorityNotifiedAt, &c.ClosedAt} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	if c.AffectedEstimate != nil {
		v := *c.AffectedEstimate
		c.AffectedEstimate = &v
	}
	return c
}

// Transfer mechanisms of a sub-processor (000093 CHECK).
var transferMechanisms = []string{"domestic", "standard_contract", "adequacy", "explicit_consent", "undecided"}

// Subprocessor is one subprocessors row (02 §16.5).
type Subprocessor struct {
	ID                  uuid.UUID
	Name                string
	Service             string
	DataCategories      []string
	Region              string
	CountryCode         string
	TransferMechanism   string
	ContractSignedAt    *civil.Date
	AuthorityNotifiedAt *civil.Date
	Active              bool
	Notes               string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (s Subprocessor) clone() Subprocessor {
	c := s
	c.DataCategories = slices.Clone(s.DataCategories)
	for _, p := range []**civil.Date{&c.ContractSignedAt, &c.AuthorityNotifiedAt} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return c
}

// RecordStore is the persistence boundary of the kvkk-2 records (05 §2.2).
// Every method works in the transaction carried by ctx, if any. Retention
// policies and sub-processors are never deleted (enabled / active = false);
// destruction logs are append-only (000091: no UPDATE/DELETE for app_rw,
// koro_forbid_mutation); breaches are deleted only by their retention
// sweeper (breach_record, BE-057).
type RecordStore interface {
	// Policies returns every retention policy, by code.
	Policies(ctx context.Context) ([]Policy, error)
	// Policy returns one policy or ErrNotFound.
	Policy(ctx context.Context, code string) (Policy, error)
	// LockPolicy returns a policy locked for the rest of the transaction.
	LockPolicy(ctx context.Context, code string) (Policy, error)
	// UpdatePolicy writes period, method, enabled, legal_approved,
	// approved_ref and updated_by; updated_at moves.
	UpdatePolicy(ctx context.Context, p Policy) error

	// InsertDestructionLog appends one destruction record row.
	InsertDestructionLog(ctx context.Context, l DestructionLog) error
	// DestructionLogs returns up to limit rows matching f, newest first
	// (created_at DESC, id DESC), after the cursor.
	DestructionLogs(ctx context.Context, f DestructionFilter, after *After, limit int) ([]DestructionLog, error)
	// DestructionLogsOfRun returns the rows of one run (erasure: the
	// request ID), oldest first.
	DestructionLogsOfRun(ctx context.Context, runID uuid.UUID) ([]DestructionLog, error)
	// PurgeDestructionLogs deletes up to limit destruction rows created
	// before cutoff, oldest first, through 000121's destruction_logs_purge
	// (the table is append-only; the database refuses a real purge of an
	// unapproved policy with 55000); dryRun counts (BE-057).
	PurgeDestructionLogs(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)

	// InsertBreach writes a breach (created_at/updated_at = now).
	InsertBreach(ctx context.Context, b Breach) error
	// Breach returns one breach or ErrNotFound.
	Breach(ctx context.Context, id uuid.UUID) (Breach, error)
	// LockBreach returns a breach locked for the rest of the transaction.
	LockBreach(ctx context.Context, id uuid.UUID) (Breach, error)
	// UpdateBreach writes every mutable column of b; updated_at moves.
	UpdateBreach(ctx context.Context, b Breach) error
	// Breaches returns the breaches (status filter optional), the most
	// recently detected first (detected_at DESC, id DESC), after the cursor;
	// up to limit rows, every row when limit ≤ 0 (the breach timer).
	Breaches(ctx context.Context, status *BreachStatus, after *After, limit int) ([]Breach, error)
	// PurgeBreaches deletes up to limit breaches closed before cutoff,
	// oldest first; dryRun counts (retention breach_record, BE-057).
	PurgeBreaches(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)

	// InsertSubprocessor writes a sub-processor (ErrDuplicateSubprocessor
	// on a taken name).
	InsertSubprocessor(ctx context.Context, s Subprocessor) error
	// Subprocessor returns one sub-processor or ErrNotFound.
	Subprocessor(ctx context.Context, id uuid.UUID) (Subprocessor, error)
	// LockSubprocessor returns a sub-processor locked for the transaction.
	LockSubprocessor(ctx context.Context, id uuid.UUID) (Subprocessor, error)
	// UpdateSubprocessor writes every mutable column of s.
	UpdateSubprocessor(ctx context.Context, s Subprocessor) error
	// Subprocessors returns the inventory by name (active filter optional).
	Subprocessors(ctx context.Context, active *bool) ([]Subprocessor, error)
}
