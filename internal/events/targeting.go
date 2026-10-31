package events

import (
	"context"
	"slices"
	"time"
	"uuid"
)

// TargetingWindow bounds the participations a company may target (BE-161,
// Q-39): an event that started earlier is no longer a dimension of any
// audience, and its participations leave the students' audience keys at
// the nightly refresh (storage limitation: a year-old attendance is the
// most a targeting may reach back to). An event that has yet to start is
// always inside the window.
const TargetingWindow = 365 * 24 * time.Hour

// Targeting is the participation read behind the event dimension of
// company targeting (BE-161, Q-39): which events an audience may name and
// which of their participations the students' audience keys hold. It is a
// system read without authorisation and returns IDs and a flag only, never
// an answer, a ticket or a check-in row. events never imports outreach or
// students: the composition root binds this contract to their ports.
type Targeting interface {
	// TargetableEvents reports which of eventIDs may be targeted now
	// (targetable); an unknown event is missing from the result (false).
	TargetableEvents(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// TargetableParticipations returns, per user among userIDs, the
	// participations of targetable events: a registration that held its
	// place (confirmed, checked in or no-show) and whether the user was
	// checked in. A user without one is missing from the result.
	TargetableParticipations(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID][]TargetableParticipation, error)
}

// TargetableParticipation is one participation a targeting may reach.
type TargetableParticipation struct {
	EventID  uuid.UUID
	Attended bool // the registration is checked in (a valid, not voided check-in)
}

// TargetableRow is one row of ParticipationStore.TargetableParticipations.
type TargetableRow struct {
	UserID   uuid.UUID
	EventID  uuid.UUID
	OrgID    uuid.UUID // the organiser: the service drops a sensitive club's events
	Attended bool
}

// targetable is the one rule of the event dimension (BE-161, Q-39): a
// published or completed event everyone sees (visibility public: never a
// club's members-only or a university-only event), neither of a sensitive
// category nor organised by a sensitive club (03 Y2: special-category data
// never targets), that started within TargetingWindow before now or has
// yet to start. A cancelled event or a draft is never targetable.
// ParticipationStore.TargetableParticipations applies the same rule but
// the organiser's sensitivity, which only directory knows.
func targetable(e Event, sensitiveOrganiser bool, now time.Time) bool {
	return (e.Status == StatusPublished || e.Status == StatusCompleted) && e.Visibility == VisibilityPublic &&
		!e.IsSensitiveCategory && !sensitiveOrganiser && !e.StartsAt.Before(now.Add(-TargetingWindow))
}

var _ Targeting = (*service)(nil)

// TargetableEvents implements Targeting: one read of the events and one of
// the organisers' sensitivity (fail closed: an error is returned, nothing
// counts as targetable).
func (s *service) TargetableEvents(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}
	evs := make(map[uuid.UUID]Event, len(eventIDs))
	for batch := range slices.Chunk(eventIDs, participationBatch) {
		got, err := s.d.Store.GetEvents(ctx, batch)
		if err != nil {
			return nil, err
		}
		for id, e := range got {
			evs[id] = e
		}
	}
	var orgIDs []uuid.UUID
	for _, e := range evs {
		if !slices.Contains(orgIDs, e.OrgID) {
			orgIDs = append(orgIDs, e.OrgID)
		}
	}
	sensitive, err := s.sensitiveOrgs(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for id, e := range evs {
		out[id] = targetable(e, sensitive[e.OrgID], now)
	}
	return out, nil
}

// TargetableParticipations implements Targeting: the store's rows of
// public, non-sensitive-category events within the window, without the
// events of sensitive clubs.
func (s *service) TargetableParticipations(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID][]TargetableParticipation, error) {
	out := map[uuid.UUID][]TargetableParticipation{}
	if len(userIDs) == 0 {
		return out, nil
	}
	since := s.now().Add(-TargetingWindow)
	var rows []TargetableRow
	for batch := range slices.Chunk(userIDs, participationBatch) {
		got, err := s.d.Store.TargetableParticipations(ctx, batch, since)
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	var orgIDs []uuid.UUID
	for _, r := range rows {
		if !slices.Contains(orgIDs, r.OrgID) {
			orgIDs = append(orgIDs, r.OrgID)
		}
	}
	sensitive, err := s.sensitiveOrgs(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if sensitive[r.OrgID] {
			continue
		}
		out[r.UserID] = append(out[r.UserID], TargetableParticipation{EventID: r.EventID, Attended: r.Attended})
	}
	return out, nil
}

// sensitiveOrgs is directory's sensitive club flag of orgIDs (one read;
// none without IDs).
func (s *service) sensitiveOrgs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	if len(orgIDs) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	return s.d.Catalog.SensitiveOrgIDs(ctx, orgIDs)
}
