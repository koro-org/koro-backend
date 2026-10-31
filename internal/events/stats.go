package events

import (
	"context"
	"fmt"
	"time"
	"uuid"
)

// Statistics is the event read of the panels (stats, BE-163; SR §8):
// events and counts only, never a registrant. It is a system read without
// authorisation: the caller authorises its own read (stats.platform.read,
// stats.club.read, stats.university.read).
type Statistics interface {
	// EventStats returns the platform's event figures: what is upcoming
	// now and what took place or was cancelled among the events that
	// start in [from, to).
	EventStats(ctx context.Context, from, to time.Time) (EventStats, error)
	// OrgEventFigures returns the events of orgIDs that start in
	// [from, to) and left the draft state, with their participation
	// figures, latest start first, at most MaxOrgEventFigures of them.
	OrgEventFigures(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time) ([]EventFigures, error)
	// OrgEventTotals returns, per org of orgIDs with at least one, the
	// published or completed events that start in [from, to) and are not
	// of a sensitive category (their participation is special category
	// data, KVKK md. 6), and the places kept by those that started by now.
	OrgEventTotals(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID]EventTotals, error)
}

// EventTotals are one org's events of a range (OrgEventTotals).
type EventTotals struct {
	Events     int
	Registered int // places kept (confirmed, checked_in, no_show) in the events that started
}

// MaxOrgEventFigures bounds OrgEventFigures (a club's or a university's
// year of events fits many times over).
const MaxOrgEventFigures = 500

// EventStats are the event figures of the platform panel. Held counts the
// published or completed events that start in the range and have started
// by the time of the read; Registered (registrations that kept their
// place: confirmed, checked_in, no_show) and CheckedIn are theirs.
type EventStats struct {
	Upcoming   int // published events that start after now
	Held       int
	Cancelled  int // cancelled events that were to start in the range
	Registered int
	CheckedIn  int
}

// EventFigures are one event of an org panel. Registered counts the
// registrations that kept their place (confirmed, checked_in, no_show),
// CheckedIn and NoShow theirs.
type EventFigures struct {
	EventID    uuid.UUID
	OrgID      uuid.UUID
	Title      string
	Status     Status
	StartsAt   time.Time
	EndsAt     time.Time
	Sensitive  bool // is_sensitive_category (special category data, KVKK md. 6)
	Registered int
	CheckedIn  int
	NoShow     int
}

// StatsStore is the persistence of Statistics, part of Store.
//
// cross-org (EventStats): platform-wide counts; OrgEventFigures is keyed
// by the org IDs the caller resolved for its own panel. No registrant
// leaves.
type StatsStore interface {
	// EventStats returns the event figures at now.
	EventStats(ctx context.Context, from, to, now time.Time) (EventStats, error)
	// OrgEventFigures returns up to limit events of orgIDs, latest start first.
	OrgEventFigures(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time, limit int) ([]EventFigures, error)
	// OrgEventTotals returns the totals of orgIDs at now.
	OrgEventTotals(ctx context.Context, orgIDs []uuid.UUID, from, to, now time.Time) (map[uuid.UUID]EventTotals, error)
}

var _ Statistics = (*service)(nil)

// EventStats implements Statistics.
func (s *service) EventStats(ctx context.Context, from, to time.Time) (EventStats, error) {
	st, err := s.d.Store.EventStats(ctx, from, to, s.now())
	if err != nil {
		return EventStats{}, fmt.Errorf("events: event stats: %w", err)
	}
	return st, nil
}

// OrgEventFigures implements Statistics.
func (s *service) OrgEventFigures(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time) ([]EventFigures, error) {
	if len(orgIDs) == 0 {
		return []EventFigures{}, nil
	}
	out, err := s.d.Store.OrgEventFigures(ctx, orgIDs, from, to, MaxOrgEventFigures)
	if err != nil {
		return nil, fmt.Errorf("events: org event figures: %w", err)
	}
	return out, nil
}

// OrgEventTotals implements Statistics.
func (s *service) OrgEventTotals(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID]EventTotals, error) {
	if len(orgIDs) == 0 {
		return map[uuid.UUID]EventTotals{}, nil
	}
	out, err := s.d.Store.OrgEventTotals(ctx, orgIDs, from, to, s.now())
	if err != nil {
		return nil, fmt.Errorf("events: org event totals: %w", err)
	}
	return out, nil
}
