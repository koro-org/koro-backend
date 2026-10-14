package ledger

import (
	"context"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/orders"
	"koro/internal/platform/queue"
)

// Late recognition of Koro's own sales (BE-067, T-293). Until BE-067 the
// capture allocated only the payee lines of a paid order: the money of
// Koro's own platform-native lines (payee NULL) stayed in buyer_advances.
// The capture now allocates them in its order_allocated; the lines paid
// before are recognised once, line by line, by this job:
// PlatformSaleRecognized, an adjustment whose business key is the order
// line (ref_type manual, ref_id = line), so a second run — or the line's
// refund, which recognises a line it finds unallocated in its own
// transaction — replays it instead of posting it again.
//
// Why a job and not a migration: the template, its checks and its
// idempotency are the ledger's Go code (one source with the capture and
// the refund); a migration would repeat them in SQL, needs a migration
// number this ticket does not own (R2) and, edited in place, would never
// run on the databases that already applied it (dev, staging). After the
// first run it finds nothing: new lines are allocated at payment.

const (
	// RecognizeJobName recognises Koro's own lines paid before their
	// template: daily at 05:05 (before the integrity check at 05:15), low.
	RecognizeJobName = "ledger.recognize_platform_sales"

	recognizeCron    = "5 5 * * *"
	recognizeUnique  = 23 * time.Hour
	recognizeTimeout = 30 * time.Minute
	// Bound each transaction, including waiting for an in-flight refund.
	recognizeLineTimeout = 30 * time.Second
)

// RecognitionReport is the result of one run of the late recognition.
type RecognitionReport struct {
	// Recognized are the lines whose sale was recognised by this run.
	Recognized []uuid.UUID
	// Skipped are lines left for finance (the allocation check of
	// ledger.integrity_check reports them every night): a line refunded
	// before its sale was recognised (the refund took the money from
	// buyer_advances: recognising the full sale would overstate it) or a
	// line whose model has no template.
	Skipped []uuid.UUID
}

func (s *service) recognizeJob() queue.Job {
	return queue.Job{
		Name: RecognizeJobName, Cron: recognizeCron, Queue: queue.Low, Unique: recognizeUnique,
		Timeout: recognizeTimeout, Handle: func(ctx context.Context) error {
			_, err := s.recognizePlatformSales(ctx)
			return err
		},
	}
}

// recognizePlatformSales posts the late recognition of every settled
// platform-native line the ledger has not allocated, each in its own
// transaction. Candidates are reread under order/line locks before any
// ledger write, matching the refund lock order and avoiding an FK/unique
// key deadlock. An allocation committed while waiting is left alone.
func (s *service) recognizePlatformSales(ctx context.Context) (RecognitionReport, error) {
	var rep RecognitionReport
	if s.d.Orders == nil {
		return rep, nil
	}
	var postErr error
	err := s.collectedPages(ctx, true, func(lines []orders.CollectedLine, alloc map[uuid.UUID]int64) bool {
		for _, l := range lines {
			if l.PayableAmount == 0 || alloc[l.LineID] != 0 {
				continue // free, or allocated (at payment or before); a partial allocation is the check's finding
			}
			id, skipped, err := s.recognizeLine(ctx, l.LineID)
			if err != nil {
				postErr = err
				return false
			}
			if skipped {
				rep.Skipped = append(rep.Skipped, l.LineID)
			}
			if id == uuid.Nil() {
				continue
			}
			rep.Recognized = append(rep.Recognized, l.LineID)
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "ledger recognised Koro's own sale late",
				slog.String("transaction_id", id.String()), slog.String("order_line_id", l.LineID.String()),
				slog.String("order_id", l.OrderID.String()), slog.Int64("payable", l.PayableAmount))
		}
		return true
	})
	if err == nil {
		err = postErr
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ledger late recognition ran",
		slog.Int("recognized", len(rep.Recognized)), slog.Int("skipped", len(rep.Skipped)), slog.Bool("ok", err == nil))
	return rep, err
}

// recognizeLine retains the order and line locks through Post and commit.
// The page's refund/allocation amounts may be stale by the time this runs.
func (s *service) recognizeLine(ctx context.Context, lineID uuid.UUID) (uuid.UUID, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, recognizeLineTimeout)
	defer cancel()
	var id uuid.UUID
	var skipped bool
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		l, err := s.d.Orders.ForAllocation(ctx, lineID)
		if err != nil {
			return err
		}
		alloc, err := s.d.Store.LineAllocations(ctx, []uuid.UUID{lineID})
		if err != nil {
			return err
		}
		if l.PayableAmount == 0 || alloc[lineID] != 0 {
			return nil
		}
		if l.RefundedAmount != 0 {
			skipped = true
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "ledger late recognition skipped: the line was refunded before its sale was recognised",
				slog.String("order_line_id", l.LineID.String()), slog.String("order_id", l.OrderID.String()),
				slog.Int64("payable", l.PayableAmount), slog.Int64("refunded", l.RefundedAmount))
			return nil
		}
		t, err := PlatformSaleRecognized(Model(l.ModelCode), AllocationOf(l.PaidLine), s.d.Clock.Now())
		if err != nil {
			skipped = true
			s.d.Log.LogAttrs(ctx, slog.LevelError, "ledger late recognition skipped: no template for the line",
				slog.String("order_line_id", l.LineID.String()), slog.String("order_id", l.OrderID.String()),
				slog.String("model_code", l.ModelCode), slog.String("error", err.Error()))
			return nil
		}
		id, err = s.Post(ctx, t)
		return err
	})
	return id, skipped, TranslateError(err)
}
