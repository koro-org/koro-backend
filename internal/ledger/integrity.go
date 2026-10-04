package ledger

import (
	"context"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/platform/queue"
)

// Job names (01 §7).
const (
	// IntegrityJobName checks the ledger every night (05:15, low queue).
	IntegrityJobName = "ledger.integrity_check"

	integrityCron    = "15 5 * * *"
	integrityUnique  = 23 * time.Hour
	integrityTimeout = 30 * time.Minute
	// findingLimit bounds each check's findings: one alarm per row, and a
	// broken ledger needs a person, not ten thousand pages.
	findingLimit = 100
)

// FindingKind classifies a Finding.
type FindingKind string

// Finding kinds.
const (
	FindingUnbalanced      FindingKind = "unbalanced_transaction" // < 2 entries or Σ ≠ 0 (the trigger was bypassed)
	FindingNegativeBalance FindingKind = "negative_payee_balance" // Koro owes a payee less than nothing
	FindingLateEntry       FindingKind = "late_entry"             // an entry written after its header's transaction (T-226)
	FindingPayout          FindingKind = "payout_mismatch"        // a paid payout and the ledger disagree
	FindingCheckFailed     FindingKind = "check_failed"           // the check itself could not run
)

// Finding is one integrity violation. It carries IDs and amounts only (no
// personal data): finance looks the rows up.
type Finding struct {
	Kind          FindingKind
	TransactionID *uuid.UUID
	EntryID       *uuid.UUID
	PayeeID       *uuid.UUID
	PayoutID      *uuid.UUID
	Amount        *int64 // the sum (unbalanced) or the balance (negative)
	Entries       int    // entry count (unbalanced)
	Detail        string // problem code or error text, no personal data
}

// IntegrityReport is the result of one run of the checks.
type IntegrityReport struct {
	Findings []Finding
}

// OK reports a clean ledger.
func (r IntegrityReport) OK() bool { return len(r.Findings) == 0 }

func (s *service) integrityJob() queue.Job {
	return queue.Job{
		Name: IntegrityJobName, Cron: integrityCron, Queue: queue.Low, Unique: integrityUnique,
		Timeout: integrityTimeout, Handle: s.integrityCheck,
	}
}

// checkIntegrity runs the four checks of 01 §7 ledger.integrity_check:
// Σ = 0 per transaction, payee balance ≥ 0, paid payouts ↔ ledger, plus
// T-226 (entries written after their header). Each check is one statement
// (one consistent snapshot); the job only reads.
func (s *service) checkIntegrity(ctx context.Context) (IntegrityReport, error) {
	var rep IntegrityReport
	unbalanced, err := s.d.Store.Unbalanced(ctx, findingLimit)
	if err != nil {
		return rep, err
	}
	for _, u := range unbalanced {
		rep.Findings = append(rep.Findings, Finding{
			Kind: FindingUnbalanced, TransactionID: ptr(u.TransactionID), Amount: u.Sum, Entries: u.Entries,
		})
	}
	negative, err := s.d.Store.NegativePayeeBalances(ctx, findingLimit)
	if err != nil {
		return rep, err
	}
	for _, b := range negative {
		bal := b.Balance
		rep.Findings = append(rep.Findings, Finding{Kind: FindingNegativeBalance, PayeeID: ptr(b.PayeeID), Amount: &bal})
	}
	late, err := s.d.Store.LateEntries(ctx, findingLimit)
	if err != nil {
		return rep, err
	}
	for _, l := range late {
		rep.Findings = append(rep.Findings, Finding{Kind: FindingLateEntry, TransactionID: ptr(l.TransactionID), EntryID: ptr(l.EntryID)})
	}
	payouts, err := s.d.Store.PayoutMismatches(ctx, findingLimit)
	if err != nil {
		return rep, err
	}
	for _, p := range payouts {
		rep.Findings = append(rep.Findings, Finding{
			Kind: FindingPayout, PayoutID: ptr(p.PayoutID), TransactionID: p.TransactionID, Detail: p.Problem,
		})
	}
	return rep, nil
}

// integrityCheck is the job: every finding raises the alarm; a failed
// check raises it too and is retried by the queue.
func (s *service) integrityCheck(ctx context.Context) error {
	rep, err := s.checkIntegrity(ctx)
	if err != nil {
		s.alarm(ctx, Finding{Kind: FindingCheckFailed, Detail: err.Error()})
		return err
	}
	for _, f := range rep.Findings {
		s.alarm(ctx, f)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ledger integrity checked",
		slog.Bool("ok", rep.OK()), slog.Int("findings", len(rep.Findings)))
	return nil
}

// alarm raises the hook and always logs the finding at ERROR with
// alarm=true (log based alerting until the hook is wired).
func (s *service) alarm(ctx context.Context, f Finding) {
	if s.d.Alarm != nil {
		s.d.Alarm(ctx, f)
	}
	attrs := []slog.Attr{slog.Bool("alarm", true), slog.String("kind", string(f.Kind))}
	for _, id := range []struct {
		key string
		v   *uuid.UUID
	}{{"transaction_id", f.TransactionID}, {"entry_id", f.EntryID}, {"payee_id", f.PayeeID}, {"payout_id", f.PayoutID}} {
		if id.v != nil {
			attrs = append(attrs, slog.String(id.key, id.v.String()))
		}
	}
	if f.Amount != nil {
		attrs = append(attrs, slog.Int64("amount", *f.Amount))
	}
	if f.Entries > 0 {
		attrs = append(attrs, slog.Int("entries", f.Entries))
	}
	if f.Detail != "" {
		attrs = append(attrs, slog.String("detail", f.Detail))
	}
	s.d.Log.LogAttrs(ctx, slog.LevelError, "ledger integrity alarm", attrs...)
}
