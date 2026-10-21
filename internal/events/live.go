package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
)

// The live check-in board (BE-054; 04 §W, 01 §1.3 platform/realtime).
//
// Who watches: exactly who may read the counters (events.checkin.stats:
// the event's staff, the organiser's managers, k-po). The board is
// authorised like getCheckinStats when the WebSocket opens (an invisible
// event 404, a visible one without the role 403, nothing upgraded) and
// again by the hub every 60 seconds and before a message to a connection
// checked more than 10 seconds ago; a removed staff member is closed with
// 4403 within a minute.
//
// What travels (04 §W): the snapshot of the counters first, then after
// every committed check-in its id, the attendee as "Ayşe K." and the
// method, after every void its id, each followed by the fresh counters.
// The attendee is the only personal datum; a viewer who may not list the
// check-ins (events.checkin.list) gets the messages without it. No
// registration, user or ticket id, no sensitivity flag, nothing of another
// event: the topic is the event's own channel rt:event:<id>:checkins.
//
// Publishing runs after the commit (db.AfterCommit), outside the scan's
// transaction; a failure is logged and never fails the scan. A replayed
// Idempotency-Key publishes nothing (the transaction did not run again).

// LivePublisher sends a board message to every viewer of a topic
// (realtime.Hub over Redis pub/sub).
type LivePublisher interface {
	Publish(ctx context.Context, topic string, msg []byte) error
}

// liveTimeout bounds the publication after a commit (counters + publish).
const liveTimeout = 3 * time.Second

// Board message types (04 §W).
const (
	liveSnapshot = "snapshot"
	liveCheckin  = "checkin"
	liveVoid     = "void"
	liveStats    = "stats"
)

// LiveTopic is the realtime topic of an event's board; on the bus it is
// the channel rt:event:<id>:checkins.
func LiveTopic(eventID uuid.UUID) string { return "event:" + eventID.String() + ":checkins" }

// LiveBoard is an authorised view of an event's board.
type LiveBoard struct {
	EventID uuid.UUID
	OrgID   uuid.UUID
	// Names: the viewer may see the attendees (events.checkin.list);
	// otherwise attendee_display is left out of the checkin messages.
	Names bool
}

// Topic is LiveTopic of the board's event.
func (b LiveBoard) Topic() string { return LiveTopic(b.EventID) }

// liveCounters is a snapshot or stats message.
type liveCounters struct {
	Type       string    `json:"type"`
	Confirmed  int       `json:"confirmed"`
	CheckedIn  int       `json:"checked_in"`
	Voided     int       `json:"voided"`
	Waitlisted int       `json:"waitlisted"`
	At         time.Time `json:"at"`
}

// liveScan is a checkin or void message.
type liveScan struct {
	Type            string        `json:"type"`
	CheckinID       string        `json:"checkin_id"`
	AttendeeDisplay string        `json:"attendee_display,omitempty"`
	Method          CheckinMethod `json:"method,omitempty"`
	At              time.Time     `json:"at"`
}

// OpenLiveBoard implements Service (events.checkin.stats, the WebSocket of
// 04 §W): the same rule as GetCheckinStats; Names is events.checkin.list
// on the same resource.
func (s *service) OpenLiveBoard(ctx context.Context, a authz.Actor, eventID uuid.UUID) (LiveBoard, error) {
	e, sens, err := s.checkinEvent(ctx, a, authz.EventsCheckinStats, eventID)
	if err != nil {
		return LiveBoard{}, err
	}
	role, err := s.staffRole(ctx, a, e)
	if err != nil {
		return LiveBoard{}, err
	}
	names := authz.Can(ctx, a, authz.EventsCheckinList, eventResource(e, sens, role)) == nil
	return LiveBoard{EventID: e.ID, OrgID: e.OrgID, Names: names}, nil
}

// LiveSnapshot implements Service: the board's first message.
func (s *service) LiveSnapshot(ctx context.Context, b LiveBoard) ([]byte, error) {
	return s.liveCounters(ctx, liveSnapshot, b.OrgID, b.EventID)
}

func (s *service) liveCounters(ctx context.Context, typ string, orgID, eventID uuid.UUID) ([]byte, error) {
	n, err := s.d.Store.CheckinCounts(ctx, orgID, eventID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(liveCounters{Type: typ, Confirmed: n.Confirmed, CheckedIn: n.CheckedIn, Voided: n.Voided,
		Waitlisted: n.Waitlisted, At: s.now()})
}

// LiveFilter returns the per-viewer rewrite of the board's messages: nil
// when the viewer sees the names, otherwise one that drops
// attendee_display.
func LiveFilter(b LiveBoard) func(msg []byte) ([]byte, bool) {
	if b.Names {
		return nil
	}
	return func(msg []byte) ([]byte, bool) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(msg, &m); err != nil {
			return nil, false // not ours: never forward what cannot be checked
		}
		if _, ok := m["attendee_display"]; !ok {
			return msg, true
		}
		delete(m, "attendee_display")
		out, err := json.Marshal(m)
		return out, err == nil
	}
}

// liveCheckedIn publishes a recorded check-in after the commit.
func (s *service) liveCheckedIn(ctx context.Context, c Checkin, userID uuid.UUID) {
	s.afterCommitLive(ctx, c.OrgID, c.EventID, func(ctx context.Context) liveScan {
		return liveScan{Type: liveCheckin, CheckinID: c.ID.String(), AttendeeDisplay: s.attendee(ctx, userID),
			Method: c.Method, At: c.ScannedAt.UTC()}
	})
}

// liveVoided publishes a voided check-in after the commit.
func (s *service) liveVoided(ctx context.Context, orgID, eventID, checkinID uuid.UUID, at time.Time) {
	s.afterCommitLive(ctx, orgID, eventID, func(context.Context) liveScan {
		return liveScan{Type: liveVoid, CheckinID: checkinID.String(), At: at.UTC()}
	})
}

// afterCommitLive publishes the message and then the fresh counters on the
// event's board once the transaction of ctx commits.
func (s *service) afterCommitLive(ctx context.Context, orgID, eventID uuid.UUID, msg func(context.Context) liveScan) {
	if s.d.Live == nil {
		return
	}
	db.AfterCommit(ctx, func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, liveTimeout)
		defer cancel()
		topic := LiveTopic(eventID)
		m, err := json.Marshal(msg(ctx))
		if err == nil {
			err = s.d.Live.Publish(ctx, topic, m)
		}
		if err == nil {
			var stats []byte
			if stats, err = s.liveCounters(ctx, liveStats, orgID, eventID); err == nil {
				err = s.d.Live.Publish(ctx, topic, stats)
			}
		}
		if err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "live board publish failed",
				slog.String("event_id", eventID.String()), slog.Any("err", err))
		}
	})
}
