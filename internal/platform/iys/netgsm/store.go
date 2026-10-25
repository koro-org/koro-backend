package netgsm

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
	"koro/internal/platform/iys"
)

// Kind tells what a stored webhook item reports.
type Kind string

// Kinds of stored items.
const (
	// KindChange is a change a recipient gave İYS itself (an "IYS_*"
	// source): the change feed.
	KindChange Kind = "change"
	// KindResult is İYS's answer to an upload (Koro's, or the brand's
	// through another Netgsm channel): Submit reads it.
	KindResult Kind = "result"
)

// Results of an upload (Netgsm's resultstatus).
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
)

// Event is one webhook item as stored (000147 iys_netgsm_events): never
// the address, the citizen's name or a free text; the user the address
// resolved to when it arrived.
type Event struct {
	// Seq is the store's insertion order (the feed cursor); 0 before Append.
	Seq int64
	// Key identifies the item for de-duplication (a redelivered webhook):
	// a SHA-256 hex digest of its fields.
	Key  string
	Kind Kind
	// Ref is Koro's reference of the upload (Netgsm's refid, echoed as
	// user_refcode); "" for a change or a foreign upload.
	Ref string
	// SubmitID is Netgsm's ID of the upload request (uid / submitid).
	SubmitID string
	// UserID is the Koro user of the address; uuid.Nil when the address is
	// no Koro user's (the item is kept as a counted, anonymous marker).
	UserID uuid.UUID
	Type   iys.Type
	Status iys.Status
	// Source is İYS's source code ("IYS_WEB", "HS_WEB"); "" when absent.
	Source string
	// ConsentAt is the consent date; zero when absent.
	ConsentAt time.Time
	// RecordedAt is when İYS recorded the item (creationdate); zero when
	// absent (a failed upload).
	RecordedAt time.Time
	// Result is ResultSuccess or ResultFailure for a KindResult; "" for a
	// change.
	Result string
	// ErrCode is İYS's code of a failed upload ("H178").
	ErrCode string
	// TransactionID is İYS's ID of the record (successful items).
	TransactionID string
	ReceivedAt    time.Time
}

// ResultQuery finds İYS's answer to one upload: by Ref, or by the user,
// type, status and consent date of the upload (an answer without
// user_refcode).
type ResultQuery struct {
	Ref       string
	UserID    uuid.UUID
	Type      iys.Type
	Status    iys.Status
	ConsentAt time.Time
}

// Store keeps the webhook items of one or more feeds (one feed per
// brand). The adapter writes them on receipt (API) and reads them in the
// worker; the GORM store shares them between the processes.
type Store interface {
	// Append stores events of feed; an event whose Key the feed holds
	// already is skipped (a redelivery). It returns how many were new.
	Append(ctx context.Context, feed string, events []Event) (int, error)
	// Changes returns up to limit KindChange events of feed after seq
	// after, received at or before until, in Seq order.
	Changes(ctx context.Context, feed string, after int64, until time.Time, limit int) ([]Event, error)
	// Result returns the latest KindResult event of feed matching q.
	Result(ctx context.Context, feed string, q ResultQuery) (Event, bool, error)
	// Purge deletes up to limit events received before before (every feed).
	Purge(ctx context.Context, before time.Time, limit int) (int, error)
}

// ---------------------------------------------------------------- in memory

// MemStore is the in-memory Store of tests. Fail makes every call fail
// (an unreachable database).
type MemStore struct {
	mu     sync.Mutex
	seq    int64
	events map[string][]Event // feed → events in Seq order
	fail   error
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store (tests only).
func NewMemStore() *MemStore { return &MemStore{events: map[string][]Event{}} }

// Fail makes every following call return err (nil: work again).
func (m *MemStore) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// All returns every stored event of feed in Seq order.
func (m *MemStore) All(feed string) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events[feed])
}

// Append implements Store.
func (m *MemStore) Append(_ context.Context, feed string, events []Event) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	added := 0
	for _, e := range events {
		if slices.ContainsFunc(m.events[feed], func(x Event) bool { return x.Key == e.Key }) {
			continue
		}
		m.seq++
		e.Seq = m.seq
		m.events[feed] = append(m.events[feed], e)
		added++
	}
	return added, nil
}

// Changes implements Store.
func (m *MemStore) Changes(_ context.Context, feed string, after int64, until time.Time, limit int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Event
	for _, e := range m.events[feed] {
		if len(out) == limit {
			break
		}
		if e.Kind == KindChange && e.Seq > after && !e.ReceivedAt.After(until) {
			out = append(out, e)
		}
	}
	return out, nil
}

// Result implements Store.
func (m *MemStore) Result(_ context.Context, feed string, q ResultQuery) (Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return Event{}, false, m.fail
	}
	evs := m.events[feed]
	for i := len(evs) - 1; i >= 0; i-- {
		if e := evs[i]; e.Kind == KindResult && q.matches(e) {
			return e, true, nil
		}
	}
	return Event{}, false, nil
}

// Purge implements Store.
func (m *MemStore) Purge(_ context.Context, before time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	n := 0
	for feed, evs := range m.events {
		m.events[feed] = slices.DeleteFunc(evs, func(e Event) bool {
			if n < limit && e.ReceivedAt.Before(before) {
				n++
				return true
			}
			return false
		})
	}
	return n, nil
}

// matches is the query of the GORM store: the reference, or the user,
// type, status and consent date (both need a reference).
func (q ResultQuery) matches(e Event) bool {
	switch {
	case q.Ref == "":
		return false
	case e.Ref == q.Ref:
		return true
	}
	return q.UserID != uuid.Nil() && e.UserID == q.UserID && e.Type == q.Type && e.Status == q.Status &&
		!q.ConsentAt.IsZero() && e.ConsentAt.Equal(q.ConsentAt)
}

// ---------------------------------------------------------------- PostgreSQL

// NewGormStore returns the PostgreSQL store over iys_netgsm_events (000147).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// eventRow scans one iys_netgsm_events row.
type eventRow struct {
	Seq           int64      `gorm:"column:seq"`
	EventKey      string     `gorm:"column:event_key"`
	Kind          string     `gorm:"column:kind"`
	Ref           *string    `gorm:"column:ref"`
	SubmitID      *string    `gorm:"column:submit_id"`
	UserID        *uuid.UUID `gorm:"column:user_id"`
	Type          string     `gorm:"column:type"`
	Status        string     `gorm:"column:status"`
	Source        string     `gorm:"column:source"`
	ConsentAt     *time.Time `gorm:"column:consent_at"`
	RecordedAt    *time.Time `gorm:"column:recorded_at"`
	Result        *string    `gorm:"column:result"`
	ErrCode       *string    `gorm:"column:err_code"`
	TransactionID *string    `gorm:"column:transaction_id"`
	ReceivedAt    time.Time  `gorm:"column:received_at"`
}

const eventCols = `seq, event_key, kind, ref, submit_id, user_id, type, status, source, consent_at, recorded_at,
	result, err_code, transaction_id, received_at`

func (r eventRow) event() Event {
	e := Event{
		Seq: r.Seq, Key: r.EventKey, Kind: Kind(r.Kind), Ref: deref(r.Ref), SubmitID: deref(r.SubmitID),
		Type: iys.Type(r.Type), Status: iys.Status(r.Status), Source: r.Source, Result: deref(r.Result),
		ErrCode: deref(r.ErrCode), TransactionID: deref(r.TransactionID), ReceivedAt: r.ReceivedAt.UTC(),
	}
	if r.UserID != nil {
		e.UserID = *r.UserID
	}
	if r.ConsentAt != nil {
		e.ConsentAt = r.ConsentAt.UTC()
	}
	if r.RecordedAt != nil {
		e.RecordedAt = r.RecordedAt.UTC()
	}
	return e
}

// Append implements Store: one INSERT … ON CONFLICT DO NOTHING per event,
// outside any transaction (a failure leaves the delivery to Netgsm's
// retry; what was stored stays de-duplicated).
//
// cross-org: platform state of the Netgsm İYS webhook (no org).
func (s gormStore) Append(ctx context.Context, feed string, events []Event) (int, error) {
	added := 0
	for _, e := range events {
		var user *uuid.UUID
		if e.UserID != uuid.Nil() {
			u := e.UserID
			user = &u
		}
		res := db.Conn(ctx, s.orm).Exec(`INSERT INTO iys_netgsm_events (feed, event_key, kind, ref, submit_id, user_id, type,
			status, source, consent_at, recorded_at, result, err_code, transaction_id, received_at)
			VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? )
			ON CONFLICT (feed, event_key) DO NOTHING`,
			feed, e.Key, string(e.Kind), nullStr(e.Ref), nullStr(e.SubmitID), user, string(e.Type), string(e.Status),
			e.Source, nullTime(e.ConsentAt), nullTime(e.RecordedAt), nullStr(e.Result), nullStr(e.ErrCode),
			nullStr(e.TransactionID), e.ReceivedAt)
		if res.Error != nil {
			return added, fmt.Errorf("iys netgsm: store event: %w", db.Translate(res.Error))
		}
		added += int(res.RowsAffected)
	}
	return added, nil
}

// Changes implements Store.
//
// cross-org: platform state of the Netgsm İYS webhook (no org).
func (s gormStore) Changes(ctx context.Context, feed string, after int64, until time.Time, limit int) ([]Event, error) {
	var rows []eventRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+eventCols+` FROM iys_netgsm_events
		WHERE feed = ? AND kind = 'change' AND seq > ? AND received_at <= ? ORDER BY seq LIMIT ?`,
		feed, after, until, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("iys netgsm: read changes: %w", err)
	}
	out := make([]Event, len(rows))
	for i, r := range rows {
		out[i] = r.event()
	}
	return out, nil
}

// Result implements Store.
//
// cross-org: platform state of the Netgsm İYS webhook (no org).
func (s gormStore) Result(ctx context.Context, feed string, q ResultQuery) (Event, bool, error) {
	var rows []eventRow
	c := db.Conn(ctx, s.orm)
	var err error
	switch {
	case q.Ref != "" && q.UserID != uuid.Nil() && !q.ConsentAt.IsZero():
		err = c.Raw(`SELECT `+eventCols+` FROM iys_netgsm_events WHERE feed = ? AND kind = 'result'
			AND (ref = ? OR (user_id = ? AND type = ? AND status = ? AND consent_at = ? ))
			ORDER BY seq DESC LIMIT 1`, feed, q.Ref, q.UserID, string(q.Type), string(q.Status), q.ConsentAt).Scan(&rows).Error
	case q.Ref != "":
		err = c.Raw(`SELECT `+eventCols+` FROM iys_netgsm_events WHERE feed = ? AND kind = 'result' AND ref = ?
			ORDER BY seq DESC LIMIT 1`, feed, q.Ref).Scan(&rows).Error
	default:
		return Event{}, false, nil // without a reference nothing matches (MemStore alike)
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("iys netgsm: read result: %w", err)
	}
	if len(rows) == 0 {
		return Event{}, false, nil
	}
	return rows[0].event(), true, nil
}

// Purge implements Store.
//
// cross-org: platform state of the Netgsm İYS webhook (no org).
func (s gormStore) Purge(ctx context.Context, before time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM iys_netgsm_events WHERE seq IN
		(SELECT seq FROM iys_netgsm_events WHERE received_at < ? ORDER BY seq LIMIT ? )`, before, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("iys netgsm: purge events: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
