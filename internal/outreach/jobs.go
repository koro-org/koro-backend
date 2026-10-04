package outreach

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"uuid"

	"koro/internal/platform/problem"
)

// recountBatch is the number of segments read and written per transaction
// (01 §7: at most 1 000 rows per transaction).
const recountBatch = 1000

// recountSegments is outreach.recount_segments (01 §7, 06:00): every
// active segment gets a fresh estimate, k < 10 → NULL. A segment that
// names a club which is sensitive now gets NULL as well: a sensitive club
// is never counted (03 Y2) and the definition no longer compiles for a
// company. It spends no count budget (a system job, no actor) and is
// idempotent: a rerun writes the same values.
func (s *service) recountSegments(ctx context.Context) error {
	after, total, changed := uuid.Nil(), 0, 0
	for ctx.Err() == nil {
		rows, err := s.d.Store.ActiveAfter(ctx, after, recountBatch)
		if err != nil {
			return fmt.Errorf("%s: %w", RecountSegmentsJobName, err)
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].ID
		n, err := s.recountRows(ctx, rows)
		if err != nil {
			return fmt.Errorf("%s: %w", RecountSegmentsJobName, err)
		}
		total, changed = total+len(rows), changed+n
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "segments recounted", slog.Int("segments", total), slog.Int("changed", changed))
	return nil
}

// recountRows counts rows (reads, outside the transaction) and writes the
// estimates in one transaction; it returns how many estimates changed.
func (s *service) recountRows(ctx context.Context, rows []Segment) (int, error) {
	var clubs []uuid.UUID
	for _, r := range rows {
		clubs = append(clubs, r.Definition.Club...)
	}
	sensitive := map[uuid.UUID]bool{}
	if len(clubs) > 0 {
		slices.SortFunc(clubs, uuid.UUID.Compare)
		var err error
		if sensitive, err = s.d.Catalog.SensitiveOrgIDs(ctx, slices.Compact(clubs)); err != nil {
			return 0, fmt.Errorf("sensitive clubs: %w", err)
		}
	}
	counts := make([]*int, len(rows))
	for i, r := range rows {
		if slices.ContainsFunc(r.Definition.Club, func(id uuid.UUID) bool { return sensitive[id] }) {
			continue
		}
		est, err := s.estimate(ctx, r)
		if err != nil {
			return 0, err
		}
		counts[i] = est.Count
	}
	now, changed := s.now(), 0
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		for i, r := range rows {
			if _, err := s.d.Store.SetCount(ctx, r.OrgID, r.ID, counts[i], now); err != nil {
				return err
			}
			if !sameCount(r.LastCount, counts[i]) {
				changed++
			}
		}
		return nil
	})
	return changed, err
}

// estimate is the published estimate of a stored segment. A definition
// students refuses (a malformed row) is logged and counts as below the
// threshold instead of stopping the whole run.
func (s *service) estimate(ctx context.Context, r Segment) (Estimate, error) {
	est, err := s.size(ctx, r.Definition)
	if errors.Is(err, problem.ErrValidation) {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "segment definition refused by students",
			slog.String("segment_id", r.ID.String()), slog.String("org_id", r.OrgID.String()))
		return Estimate{BelowThreshold: true}, nil
	}
	return est, err
}

func sameCount(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
