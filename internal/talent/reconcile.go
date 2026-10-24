package talent

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/queue"
)

// Reconciliation of a closed period (00 T-321; BE-090, user decision of
// 2026-10-23). While talent.default_enabled is false nothing is written
// (T-32): a registration cancelled, a check-in voided, a like withdrawn or
// a club membership ended in that period leaves the row its credit wrote
// while the flag was on without its reversal. Nothing of the period is
// processed while the flag is off (no source is read, no row written); the
// reversal consumers only count their drop in talent.collection_state
// (000138; no personal data). The first talent.recompute with collection
// on after a drop reconciles once, before it scores: it walks the
// originals of the four reversible signals no reversal points at, asks
// the source modules through their ports which sources were undone (events:
// a cancelled registration, a voided check-in; feed: a withdrawn like of a
// post that still exists; orgs: an ended membership), writes the
// consumer's reversal row for each and records the drop count it started
// from. A drop during the run stays pending for the next night; a second
// run over the same state changes nothing (no drop pending; a reversal is
// one per row, point_events_reversal_uq). Credits dropped in the period
// are never back-filled (T-32: nothing is processed while off).

// CollectionState is talent.collection_state (000138).
type CollectionState struct {
	ReversalsDropped int64      // reversal events dropped with collection off, ever
	LastDroppedAt    *time.Time // the latest drop
	ReconciledDrops  int64      // the ReversalsDropped the last reconciliation started from
	ReconciledAt     *time.Time // when it finished
}

// Pending reports drops no reconciliation covered.
func (c CollectionState) Pending() bool { return c.ReversalsDropped > c.ReconciledDrops }

// reconciledSignals are the signals a consumer reverses, with the reason of
// the reversal row (the consumer's: the reconciliation reverses for the
// same undoing; its rows carry the run's ID as source_event_id).
var reconciledSignals = map[string]string{
	SignalRegistrationConfirmed: reasonRegistrationCancelled,
	SignalEventCheckin:          reasonCheckinVoided,
	SignalFeedLike:              reasonPostUnliked,
	SignalClubMembership:        reasonMembershipEnded,
}

// reconcileBatch is the ledger rows per walk step, port read and talent
// transaction (01 §7: ≤ 1000 rows per transaction).
const reconcileBatch = RetentionBatch

// reconcile writes the reversals of the sources undone while collection
// was off and returns how many it wrote. It runs as the nightly job
// (talent.ledger.write: it appends ledger rows). The source reads run on
// app_rw between the talent transactions.
func (s *service) reconcile(ctx context.Context) (int, error) {
	if err := systemAction(ctx, RecomputeJobName, authz.TalentLedgerWrite); err != nil {
		return 0, queue.Permanent(err)
	}
	var st CollectionState
	if err := s.inTx(ctx, func(ctx context.Context) error {
		var err error
		st, err = s.d.Store.CollectionState(ctx)
		return err
	}); err != nil {
		return 0, err
	}
	if !st.Pending() {
		return 0, nil
	}
	run := uuid.NewV7() // source_event_id of the run's reversals (the trace)
	signals := slices.Sorted(maps.Keys(reconciledSignals))
	written, checked := 0, 0
	for after := uuid.Nil(); ; {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		var rows []PointEvent
		if err := s.inTx(ctx, func(ctx context.Context) error {
			var err error
			rows, err = s.d.Store.Unreversed(ctx, signals, after, reconcileBatch)
			return err
		}); err != nil {
			return written, err
		}
		if len(rows) == 0 {
			break
		}
		after, checked = rows[len(rows)-1].ID, checked+len(rows)
		undone, err := s.undone(ctx, rows)
		if err != nil {
			return written, err
		}
		n, err := s.writeReversals(ctx, undone, run)
		written += n
		if err != nil {
			return written, err
		}
		if len(rows) < reconcileBatch {
			break
		}
	}
	if err := s.inTx(ctx, func(ctx context.Context) error {
		return s.d.Store.FinishReconcile(ctx, st.ReversalsDropped, s.now())
	}); err != nil {
		return written, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "talent ledger reconciled after collection was off (T-321)",
		slog.String("run_id", run.String()), slog.Int64("drops", st.ReversalsDropped-st.ReconciledDrops),
		slog.Int("checked", checked), slog.Int("reversals", written))
	return written, nil
}

// undone returns the rows among rows whose source was undone, read from
// the source modules.
func (s *service) undone(ctx context.Context, rows []PointEvent) ([]PointEvent, error) {
	var sources, memberships []uuid.UUID
	var likes []LikeRef
	for _, r := range rows {
		switch r.Signal {
		case SignalRegistrationConfirmed, SignalEventCheckin:
			sources = append(sources, r.SourceID)
		case SignalClubMembership:
			memberships = append(memberships, r.SourceID)
		case SignalFeedLike:
			likes = append(likes, LikeRef{UserID: r.UserID, PostID: r.SourceID})
		}
	}
	events, ended, withdrawn := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}, map[LikeRef]bool{}
	var err error
	if len(sources) > 0 {
		if events, err = s.d.Events.Undone(ctx, sources); err != nil {
			return nil, fmt.Errorf("talent: undone registrations and check-ins: %w", err)
		}
	}
	if len(memberships) > 0 {
		if ended, err = s.d.Memberships.EndedMemberships(ctx, memberships); err != nil {
			return nil, fmt.Errorf("talent: ended memberships: %w", err)
		}
	}
	if len(likes) > 0 {
		if withdrawn, err = s.d.Likes.WithdrawnLikes(ctx, likes); err != nil {
			return nil, fmt.Errorf("talent: withdrawn likes: %w", err)
		}
	}
	var out []PointEvent
	for _, r := range rows {
		var gone bool
		switch r.Signal {
		case SignalRegistrationConfirmed, SignalEventCheckin:
			gone = events[r.SourceID]
		case SignalClubMembership:
			gone = ended[r.SourceID]
		case SignalFeedLike:
			gone = withdrawn[LikeRef{UserID: r.UserID, PostID: r.SourceID}]
		}
		if gone {
			out = append(out, r)
		}
	}
	return out, nil
}

// writeReversals appends the reversal of every row in one talent
// transaction and returns how many it wrote; a row a consumer reversed
// meanwhile is a duplicate. The outcomes are counted after the commit.
func (s *service) writeReversals(ctx context.Context, rows []PointEvent, run uuid.UUID) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var outcomes []string
	err := s.inTx(ctx, func(ctx context.Context) error {
		outcomes = outcomes[:0]
		for _, r := range rows {
			_, inserted, err := s.d.Store.InsertPoint(ctx, reversalOf(r, reconciledSignals[r.Signal], run))
			if err != nil {
				return err
			}
			outcome := OutcomeReconciled
			if !inserted {
				outcome = OutcomeDuplicate
			}
			outcomes = append(outcomes, outcome)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for i, o := range outcomes {
		s.counts.inc(rows[i].Signal, o)
		if o == OutcomeReconciled {
			n++
		}
	}
	return n, nil
}
