package students

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// The event dimension of the audience (BE-161, Q-39): a company targets the
// students who registered for ("event:<id>": a confirmed, checked-in or
// no-show registration) or attended ("attended:<id>": checked in) a
// targetable event. Guarded on every side:
//
//   - only targetable events (events' rule: a published or completed public
//     event, neither of a sensitive category nor organised by a sensitive
//     club, that started within the last year or has yet to start): a
//     club's members-only or a university-only event, a cancelled event and
//     a draft never become keys;
//   - only students whose commercial message consent (any channel) is in
//     force: the participations of the others never become keys, so they
//     are neither counted nor reached through this dimension;
//   - the keys are derived like every other audience key (the event path
//     and the nightly refresh agree), counted only through count_segment
//     (k ≥ 10, no number below) and never shown to a company;
//   - a spec naming an event that is no longer targetable matches nobody
//     through it (compile), whatever stale key a profile still holds.
const (
	keyEvent    = "event:"
	keyAttended = "attended:"
)

// EventKey is the audience key of a registration for eventID.
func EventKey(eventID uuid.UUID) string { return keyEvent + eventID.String() }

// AttendedKey is the audience key of an attendance (check-in) of eventID.
func AttendedKey(eventID uuid.UUID) string { return keyAttended + eventID.String() }

// Worker task names of the event keys (01 §6.2; <module>.<verb>_<object>).
const (
	// AddEventKeyTaskName consumes registration.confirmed.
	AddEventKeyTaskName = "students.add_event_key"
	// RemoveEventKeyTaskName consumes registration.cancelled.
	RemoveEventKeyTaskName = "students.remove_event_key"
	// AddAttendedKeyTaskName consumes checkin.recorded.
	AddAttendedKeyTaskName = "students.add_attended_key"
	// RemoveAttendedKeyTaskName consumes checkin.voided.
	RemoveAttendedKeyTaskName = "students.remove_attended_key"
	// ApplyMarketingConsentTaskName consumes consent.recorded: a commercial
	// consent given or withdrawn adds or removes the student's event keys.
	ApplyMarketingConsentTaskName = "students.apply_marketing_consent"
	// DropEventKeysTaskName consumes event.updated: an event that is no
	// longer targetable (its visibility, sensitive category or start
	// changed) leaves every profile's keys at once.
	DropEventKeysTaskName = "students.drop_event_keys"
)

// marketingPurposes are the commercial message consents (6563; 02 §4.1):
// any of them in force opens a student's participations to the event
// dimension (Q-39: "pazarlama/iletişim izni").
var marketingPurposes = []consent.Purpose{
	consent.PurposeCommercialEmail, consent.PurposeCommercialPush, consent.PurposeCommercialSMS,
}

// targetingFields are the event fields whose change may make an event
// untargetable (events.targetable).
var targetingFields = []string{"visibility", "is_sensitive_category", "starts_at"}

// participations returns the bound events reader, or nil.
func (s *service) participations() Participations {
	if s.d.Participations == nil {
		return nil
	}
	return s.d.Participations()
}

// eventFacts returns the targetable participations of the users among ids
// whose commercial consent is in force; none without the consent checker
// or the events reader (fail closed).
func (s *service) eventFacts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]EventParticipation, error) {
	r := s.participations()
	if r == nil || s.d.Consents == nil || len(ids) == 0 {
		return map[uuid.UUID][]EventParticipation{}, nil
	}
	in := map[uuid.UUID]bool{}
	for _, p := range marketingPurposes {
		got, err := s.d.Consents.FilterActive(ctx, ids, p)
		if err != nil {
			return nil, fmt.Errorf("students: marketing consent: %w", err)
		}
		for _, id := range got {
			in[id] = true
		}
	}
	consenting := make([]uuid.UUID, 0, len(in))
	for _, id := range ids {
		if in[id] {
			consenting = append(consenting, id)
		}
	}
	if len(consenting) == 0 {
		return map[uuid.UUID][]EventParticipation{}, nil
	}
	out, err := r.TargetableParticipations(ctx, consenting)
	if err != nil {
		return nil, fmt.Errorf("students: participations: %w", err)
	}
	return out, nil
}

// eventKeys are the keys of parts: event:<id> for each, attended:<id> for
// each checked-in one.
func eventKeys(parts []EventParticipation) []string {
	var keys []string
	for _, p := range parts {
		keys = append(keys, EventKey(p.EventID))
		if p.Attended {
			keys = append(keys, AttendedKey(p.EventID))
		}
	}
	return keys
}

// isEventKey reports an event or attended key.
func isEventKey(k string) bool {
	return strings.HasPrefix(k, keyEvent) || strings.HasPrefix(k, keyAttended)
}

// eventOf returns the event of an event or attended key.
func eventOf(k string) (uuid.UUID, bool) {
	v, ok := strings.CutPrefix(k, keyEvent)
	if !ok {
		if v, ok = strings.CutPrefix(k, keyAttended); !ok {
			return uuid.Nil(), false
		}
	}
	id, err := uuid.Parse(v)
	return id, err == nil
}

// dropUntargetable removes from keys the event and attended keys of events
// that are not targetable now (a spec's guard: a stale stored key never
// reaches anyone through an event that became members-only, sensitive,
// cancelled or too old). Without the events reader every event key goes.
// The caller keeps the dimension count, so removing a key never widens the
// audience.
func (s *service) dropUntargetable(ctx context.Context, keys []string) ([]string, error) {
	var evs []uuid.UUID
	for _, k := range keys {
		if id, ok := eventOf(k); ok && !slices.Contains(evs, id) {
			evs = append(evs, id)
		}
	}
	if len(evs) == 0 {
		return keys, nil
	}
	ok := map[uuid.UUID]bool{}
	if r := s.participations(); r != nil {
		var err error
		if ok, err = r.TargetableEvents(ctx, evs); err != nil {
			return nil, fmt.Errorf("students: targetable events: %w", err)
		}
	}
	return slices.DeleteFunc(keys, func(k string) bool {
		id, isEvent := eventOf(k)
		return isEvent && !ok[id]
	}), nil
}

// subscribeEventKeys registers the event key subscriptions (Subscribe).
func (m Module) subscribeEventKeys(r *outbox.Registry, q outbox.Option) {
	r.Subscribe(domainevent.TopicRegistrationConfirmed, AddEventKeyTaskName, m.svc.handleRegistrationConfirmed, q)
	r.Subscribe(domainevent.TopicRegistrationCancelled, RemoveEventKeyTaskName, m.svc.handleRegistrationCancelled, q)
	r.Subscribe(domainevent.TopicCheckinRecorded, AddAttendedKeyTaskName, m.svc.handleCheckinRecorded, q)
	r.Subscribe(domainevent.TopicCheckinVoided, RemoveAttendedKeyTaskName, m.svc.handleCheckinVoided, q)
	r.Subscribe(domainevent.TopicConsentRecorded, ApplyMarketingConsentTaskName, m.svc.handleMarketingConsent, q)
	r.Subscribe(domainevent.TopicEventUpdated, DropEventKeysTaskName, m.svc.handleEventUpdated, q)
}

// handleRegistrationConfirmed is students.add_event_key
// (registration.confirmed): the student's keys are recomputed. A sensitive
// event never becomes a key.
func (s *service) handleRegistrationConfirmed(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationConfirmed](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Sensitive {
		return nil
	}
	return s.refreshUser(ctx, p.UserID, AddEventKeyTaskName)
}

// handleRegistrationCancelled is students.remove_event_key
// (registration.cancelled; an event's cancellation cancels each
// registration).
func (s *service) handleRegistrationCancelled(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationCancelled](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Sensitive {
		return nil
	}
	return s.refreshUser(ctx, p.UserID, RemoveEventKeyTaskName)
}

// handleCheckinRecorded is students.add_attended_key (checkin.recorded).
func (s *service) handleCheckinRecorded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.CheckinRecorded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Sensitive {
		return nil
	}
	return s.refreshUser(ctx, p.UserID, AddAttendedKeyTaskName)
}

// handleCheckinVoided is students.remove_attended_key (checkin.voided: the
// registration is confirmed again, its attended key goes).
func (s *service) handleCheckinVoided(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.CheckinVoided](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.refreshUser(ctx, p.UserID, RemoveAttendedKeyTaskName)
}

// handleMarketingConsent is students.apply_marketing_consent
// (consent.recorded): a commercial consent given or withdrawn recomputes
// the student's keys (a withdrawal of the last one removes every event
// key; another purpose is ignored).
func (s *service) handleMarketingConsent(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ConsentRecorded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if !slices.Contains(marketingPurposes, consent.Purpose(p.Purpose)) {
		return nil
	}
	return s.refreshUser(ctx, p.UserID, ApplyMarketingConsentTaskName)
}

// handleEventUpdated is students.drop_event_keys (event.updated): when an
// edit of the visibility, the sensitive category or the start left the
// event untargetable, its keys leave every profile now (not at the
// nightly refresh). An event that became targetable reaches its
// participants at the nightly refresh. Idempotent.
func (s *service) handleEventUpdated(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.EventUpdated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if !slices.ContainsFunc(p.Fields, func(f string) bool { return slices.Contains(targetingFields, f) }) {
		return nil
	}
	if r := s.participations(); r != nil {
		ok, err := r.TargetableEvents(ctx, []uuid.UUID{p.EventID})
		if err != nil {
			return fmt.Errorf("%s: %w", DropEventKeysTaskName, err)
		}
		if ok[p.EventID] {
			return nil
		}
	}
	total := 0
	for _, key := range []string{EventKey(p.EventID), AttendedKey(p.EventID)} {
		n, err := db.Drain(ctx, batchSize, func(ctx context.Context) (int, error) {
			return s.profiles().RemoveAudienceKey(ctx, key, s.now(), batchSize)
		})
		if err != nil {
			return fmt.Errorf("%s: %w", DropEventKeysTaskName, err)
		}
		total += n
	}
	if total > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "untargetable event keys removed from audiences",
			slog.String("event_id", p.EventID.String()), slog.Int("keys", total))
	}
	return nil
}
