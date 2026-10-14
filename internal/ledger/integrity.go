package ledger

import (
	"context"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/orders"
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
	FindingUnallocatedLine FindingKind = "unallocated_line"       // a settled order line the ledger did not allocate in full (BE-067)
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
	OrderLineID   *uuid.UUID // unallocated_line
	Amount        *int64     // the sum (unbalanced), the balance (negative) or what was allocated (unallocated_line)
	Entries       int        // entry count (unbalanced)
	Detail        string     // problem code or error text, no personal data
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

// checkIntegrity runs the checks of 01 §7 ledger.integrity_check: Σ = 0
// per transaction, payee balance ≥ 0, paid payouts ↔ ledger, plus T-226
// (entries written after their header) and the allocation of every settled
// order line, Koro's own platform-native lines included (BE-067). Each of
// the first four is one statement (one consistent snapshot); the
// allocation check reads orders, then the ledger, page by page (a line is
// paid and allocated in one commit, so the later ledger read always sees
// the allocation of a line the orders read saw paid). The job only reads.
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
	lines, err := s.unallocatedLines(ctx, findingLimit)
	if err != nil {
		return rep, err
	}
	rep.Findings = append(rep.Findings, lines...)
	return rep, nil
}

// Allocation problems (Finding.Detail of unallocated_line).
const (
	AllocationMissing  = "missing_allocation"  // nothing allocated: the money is still in buyer_advances
	AllocationMismatch = "allocation_mismatch" // allocated, but not the line's payable amount
)

// collectedBatch is the page of settled lines the allocation reads go by.
const collectedBatch = 500

// unallocatedLines finds the settled order lines (orders Koro collected in
// full) whose allocation in the ledger is not their payable amount: Σ of
// the buyer_advances entries naming the line in order_allocated and
// adjustment transactions (02 §21.3; BE-067: Koro's own line too, at
// payment or by its late recognition). Without the orders port (a
// poster-only ledger) there is nothing to compare with.
func (s *service) unallocatedLines(ctx context.Context, limit int) ([]Finding, error) {
	var out []Finding
	if s.d.Orders == nil {
		return out, nil
	}
	err := s.collectedPages(ctx, false, func(lines []orders.CollectedLine, alloc map[uuid.UUID]int64) bool {
		for _, l := range lines {
			got := alloc[l.LineID]
			if got == l.PayableAmount {
				continue
			}
			detail := AllocationMismatch
			if got == 0 {
				detail = AllocationMissing
			}
			out = append(out, Finding{Kind: FindingUnallocatedLine, OrderLineID: ptr(l.LineID), Amount: &got, Detail: detail})
			if len(out) == limit {
				return false
			}
		}
		return true
	})
	return out, err
}

// collectedPages reads the settled lines page by page (all or Koro's own
// only) with what the ledger allocated of each and hands every page to
// visit until it returns false.
func (s *service) collectedPages(ctx context.Context, platformOnly bool,
	visit func(lines []orders.CollectedLine, alloc map[uuid.UUID]int64) bool) error {
	after := uuid.Nil()
	for {
		lines, err := s.d.Orders.CollectedLines(ctx, after, platformOnly, collectedBatch)
		if err != nil || len(lines) == 0 {
			return err
		}
		ids := make([]uuid.UUID, len(lines))
		for i, l := range lines {
			ids[i] = l.LineID
		}
		alloc, err := s.d.Store.LineAllocations(ctx, ids)
		if err != nil {
			return err
		}
		if !visit(lines, alloc) || len(lines) < collectedBatch {
			return nil
		}
		after = lines[len(lines)-1].LineID
	}
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
	}{{"transaction_id", f.TransactionID}, {"entry_id", f.EntryID}, {"payee_id", f.PayeeID}, {"payout_id", f.PayoutID},
		{"order_line_id", f.OrderLineID}} {
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
