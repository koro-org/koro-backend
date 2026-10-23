package talent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"uuid"
)

// The gaming thresholds of a config version (02 §15.1, R-003 §3.4; BE-085).
// Each is off when absent or 0:
//
//   - min_account_age_days_for_likes: a like earns points only from an
//     account at least that many days old at the like. The consumer writes
//     nothing for a younger account (outcome young_account) and the nightly
//     score drops such a like (a row written before the threshold rose).
//   - min_event_confirmed, min_event_distinct_checkins: the nightly score
//     drops the event signals (registration, check-in, no-show) of an event
//     with fewer confirmed registrations or fewer distinct users checked in.
//     The figures change until the event is over, so only the nightly score
//     applies them, with the figures of the run: an upcoming event earns
//     nothing yet, and an event without check-in earns nothing while
//     min_event_distinct_checkins is set. An unknown source or event fails.
//
// A dropped row takes its reversal with it (the pair cancels anyway; alone
// the reversal would count against the user). The ledger keeps every row:
// the thresholds decide what the score counts, not what is recorded.

// eventSignals are the signals whose source is a registration or a
// check-in of an event.
var eventSignals = []string{SignalRegistrationConfirmed, SignalEventCheckin, SignalEventNoShow}

// gamingOf decodes the params of a stored version without validating them
// (a consumer applies the thresholds; the nightly run validates the whole
// version). Empty params have no threshold.
func gamingOf(raw json.RawMessage) (ScoreParams, error) {
	var p ScoreParams
	if len(raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ScoreParams{}, fmt.Errorf("talent: config version params: %w", err)
	}
	return p, nil
}

// minDays is a threshold's value; nil or not positive is 0 (off).
func minDays(v *int) int {
	if v == nil || *v < 0 {
		return 0
	}
	return *v
}

// oldEnough reports an account created at created that was at least days
// old at at; an unknown account (nil) is not.
func oldEnough(created *time.Time, at time.Time, days int) bool {
	return created != nil && !at.Before(created.Add(time.Duration(days)*24*time.Hour))
}

// eventFigure is the figures of one event a run read (known = false: an
// unknown event).
type eventFigure struct {
	facts EventFacts
	known bool
}

// eventFigures caches the events one run has read.
type eventFigures map[uuid.UUID]eventFigure

// screenRows returns rows without the rows the thresholds of p drop and
// their reversals. Rows of the users in skip (neutralised: never
// aggregated) are kept as they are. The reads run on app_rw, outside a
// talent transaction; figures caches the events across the batches of a
// run.
func (s *service) screenRows(ctx context.Context, p ScoreParams, figures eventFigures, rows []PointEvent,
	skip map[uuid.UUID]bool,
) ([]PointEvent, error) {
	ageDays, minConf, minCheck := minDays(p.MinAccountAgeDaysForLikes), minDays(p.MinEventConfirmed),
		minDays(p.MinEventDistinctCheckins)
	if ageDays == 0 && minConf == 0 && minCheck == 0 {
		return rows, nil
	}
	screened := func(r PointEvent) bool { return r.ReversalOf == nil && !skip[r.UserID] }
	likes := ageDays > 0
	events := minConf > 0 || minCheck > 0

	var likers, sources []uuid.UUID
	for _, r := range rows {
		switch {
		case !screened(r):
		case likes && r.Signal == SignalFeedLike:
			likers = append(likers, r.UserID)
		case events && slices.Contains(eventSignals, r.Signal):
			sources = append(sources, r.SourceID)
		}
	}
	created := map[uuid.UUID]time.Time{}
	if len(likers) > 0 {
		var err error
		if created, err = s.d.Accounts.AccountAge(ctx, uniqueIDs(likers)); err != nil {
			return nil, fmt.Errorf("talent: account age: %w", err)
		}
	}
	eventOf := map[uuid.UUID]uuid.UUID{}
	if len(sources) > 0 {
		var err error
		if eventOf, err = s.d.Events.EventsOf(ctx, uniqueIDs(sources)); err != nil {
			return nil, fmt.Errorf("talent: events of the sources: %w", err)
		}
		if err := s.readFigures(ctx, figures, eventOf); err != nil {
			return nil, err
		}
	}

	drop := map[uuid.UUID]bool{}
	for _, r := range rows {
		switch {
		case !screened(r):
		case likes && r.Signal == SignalFeedLike:
			at, ok := created[r.UserID]
			var c *time.Time
			if ok {
				c = &at
			}
			drop[r.ID] = !oldEnough(c, r.OccurredAt, ageDays)
		case events && slices.Contains(eventSignals, r.Signal):
			ev, ok := eventOf[r.SourceID]
			fig := figures[ev]
			drop[r.ID] = !ok || !fig.known || fig.facts.Confirmed < minConf || fig.facts.DistinctCheckins < minCheck
		}
	}
	out := make([]PointEvent, 0, len(rows))
	for _, r := range rows {
		if drop[r.ID] || (r.ReversalOf != nil && drop[*r.ReversalOf]) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// readFigures adds the figures of the events of eventOf that figures does
// not hold yet.
func (s *service) readFigures(ctx context.Context, figures eventFigures, eventOf map[uuid.UUID]uuid.UUID) error {
	var missing []uuid.UUID
	for _, ev := range eventOf {
		if _, seen := figures[ev]; !seen {
			missing = append(missing, ev)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	missing = uniqueIDs(missing)
	got, err := s.d.Events.EventFacts(ctx, missing)
	if err != nil {
		return fmt.Errorf("talent: event facts: %w", err)
	}
	for _, ev := range missing {
		f, ok := got[ev]
		figures[ev] = eventFigure{facts: f, known: ok}
	}
	return nil
}

// uniqueIDs returns ids sorted without duplicates (a deterministic read).
func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, compareUUID)
	return slices.Compact(out)
}
