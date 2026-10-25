package events

import (
	"context"
	"log/slog"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/db"
)

// T-264 (BE-033): nothing wrote events.status = completed. The system now
// completes a published event once it has ended:
//
//   - events.complete_ended (every hour at :40) turns published events that
//     ended at or before now into completed, in batches of 1000, each
//     audited as the system path events.event.complete;
//   - a completed event is final like before (no edit, no registration, no
//     cancellation); its check-in, attendance closing and no-show marking
//     work as for a published one (they already accepted completed), and
//     the public list keeps showing it for a date range that covers it
//     (ListEvents reads published and completed; without a range ended
//     events were left out already);
//   - no outbox event: nothing consumes the transition yet (talent signals
//     come from check-ins, BE-052).
const (
	// CompleteEndedJobName completes the published events that have ended.
	CompleteEndedJobName = "events.complete_ended"

	// auditEventComplete is the audit action of the system transition (a
	// system path, not an HTTP action: 03 §3.7 has no row for it).
	auditEventComplete = "events.event.complete"
)

// completeEnded is events.complete_ended. A row another transaction holds
// (an organiser cancelling at the same moment) is skipped and picked up on
// the next run.
func (s *service) completeEnded(ctx context.Context) error {
	sys := authz.SystemActor(CompleteEndedJobName)
	total, err := db.Batches(ctx, s.d.Tx, jobBatch, func(ctx context.Context) (int, error) {
		rows, err := s.d.Store.CompleteEnded(ctx, s.now(), jobBatch)
		if err != nil {
			return 0, err
		}
		for _, e := range rows {
			ev := auditEvent(sys, authz.Action(auditEventComplete), eventAggType, e.ID, e.OrgID, "",
				map[string]any{"ended_at": e.EndsAt.UTC().Format(time.RFC3339)})
			if err := s.d.Audit.Log(ctx, ev); err != nil {
				return 0, err
			}
		}
		return len(rows), nil
	})
	if err != nil {
		return err
	}
	if total > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ended events completed", slog.Int("count", total))
	}
	return nil
}
