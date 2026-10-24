package events

import (
	"context"
	"slices"
	"uuid"
)

// Participation is the participation read of other modules (BE-085): the
// figures behind talent's gaming thresholds (02 §15.1
// min_event_confirmed, min_event_distinct_checkins; R-003 §3.4 "at least 5
// confirmed attendees and 3 distinct users' check-ins"), the organiser
// of an event and its door staff (BE-090), and which registrations and
// check-ins were undone (BE-090). events never imports talent: the composition root adapts
// this contract to talent's port. It is a system read without
// authorisation and returns IDs and counts only, never a registrant.
type Participation interface {
	// EventsOf maps registration and check-in IDs to their event; an
	// unknown ID is missing from the result.
	EventsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
	// ParticipationFacts returns the figures of the events among eventIDs
	// (any status); an unknown event is missing from the result.
	ParticipationFacts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]ParticipationFacts, error)
	// OnStaff reports whether userID is on the door staff (event_staff,
	// any role) of event eventID of orgID (BE-090: talent's staff rule,
	// 00 T-341). An unknown event, or one of another org, is false.
	OnStaff(ctx context.Context, orgID, eventID, userID uuid.UUID) (bool, error)
	// Undone returns the registrations and check-ins among ids that were
	// undone: a cancelled registration (the transition that publishes
	// registration.cancelled; a withdrawal is not one) and a voided check-in
	// (checkin.voided). An unknown ID is missing (BE-090: talent's
	// reconciliation of a closed period, 00 T-321).
	Undone(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// ParticipationFacts are the participation figures of one event.
type ParticipationFacts struct {
	OrgID uuid.UUID // the organiser (events.org_id)
	// Confirmed counts the registrations that were confirmed and kept
	// their place: confirmed, checked_in and no_show (cancelled, rejected,
	// waitlisted, expired and withdrawn ones do not count).
	Confirmed int
	// DistinctCheckins counts the distinct users with a valid (not voided)
	// check-in: the registrations in checked_in (a void moves the
	// registration back to confirmed).
	DistinctCheckins int
}

// ParticipationStore is the persistence of Participation, part of Store.
//
// cross-org: the reads are keyed by IDs another module holds (talent's
// ledger rows name a registration or a check-in, and their events); no
// row content but the event, the organiser org, the counts and whether a
// row was undone leaves.
type ParticipationStore interface {
	// EventsOf returns the event of every registration or check-in among
	// ids that exists.
	EventsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
	// ParticipationFacts returns the figures of every event among eventIDs
	// that exists.
	ParticipationFacts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]ParticipationFacts, error)
	// Undone returns the cancelled registrations and the voided check-ins
	// among ids.
	Undone(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// participationBatch bounds the IDs of one store read.
const participationBatch = 1000

var _ Participation = (*service)(nil)

// EventsOf implements Participation.
func (s *service) EventsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := make(map[uuid.UUID]uuid.UUID, len(ids))
	for batch := range slices.Chunk(ids, participationBatch) {
		got, err := s.d.Store.EventsOf(ctx, batch)
		if err != nil {
			return nil, err
		}
		for id, ev := range got {
			out[id] = ev
		}
	}
	return out, nil
}

// ParticipationFacts implements Participation.
func (s *service) ParticipationFacts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]ParticipationFacts, error) {
	out := make(map[uuid.UUID]ParticipationFacts, len(eventIDs))
	for batch := range slices.Chunk(eventIDs, participationBatch) {
		got, err := s.d.Store.ParticipationFacts(ctx, batch)
		if err != nil {
			return nil, err
		}
		for id, f := range got {
			out[id] = f
		}
	}
	return out, nil
}

// OnStaff implements Participation (the staff row is read in the event's
// org, 06 H-38).
func (s *service) OnStaff(ctx context.Context, orgID, eventID, userID uuid.UUID) (bool, error) {
	if orgID == uuid.Nil() {
		return false, nil
	}
	_, ok, err := s.d.Store.StaffRole(ctx, orgID, eventID, userID)
	return ok, err
}

// Undone implements Participation.
func (s *service) Undone(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	for batch := range slices.Chunk(ids, participationBatch) {
		got, err := s.d.Store.Undone(ctx, batch)
		if err != nil {
			return nil, err
		}
		for id, v := range got {
			out[id] = v
		}
	}
	return out, nil
}

// heldPlace reports a registration that counts in ParticipationFacts.Confirmed.
func heldPlace(st RegStatus) bool { return st == RegConfirmed || st == RegCheckedIn || st == RegNoShow }
