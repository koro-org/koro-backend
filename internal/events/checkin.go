package events

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// Check-in at the door (BE-031; ADR-0013, 01 §6.3 "Check-in", 02 §11.8,
// 04 §E.5, R-003 §2.5.5, 00 T-70).
//
// Who scans: the event's staff (scanner or manager), the organiser's
// club_admin / university_rep / career_staff and, for the platform org,
// platform_admin (03 §5.6: S, E, k-po). The event is the resource
// (sub-surface, 03 §7.6): an invisible event 404, a visible one without
// the role 403, before any ticket is read.
//
// The chain, in one transaction with the Idempotency-Key (06 H-5):
//
//	the event row FOR KEY SHARE (published, check-in on, attendance open;
//	else 409) → a retry of the same client_scan_id by the same staff
//	member for the same event returns the first answer (T-137) → the
//	ticket: the QR token's SHA-256 is looked up and the token verified
//	against the row (re-derived, constant time) or the manual code is
//	looked up in the event → the conditional UPDATE confirmed → checked_in
//	(one winner among concurrent scans) → event_checkins (partial unique:
//	the second layer) → audit → outbox checkin.recorded.
//
// Lock order: the event row FOR KEY SHARE first. Scans share it; every
// flow that changes a registration's seat or ticket (cancellation, the
// organiser's decisions, reissue, event.cancelled, erasure, closing the
// attendance) holds it FOR UPDATE, so a scan reads the ticket and the
// registration only after such a change committed, and the change waits
// for the scans in flight. Concurrent scans then queue on the
// registration row (the conditional UPDATE).
//
// Business outcomes are 200 with result (T-70): ok, already_checked_in
// (with the first check-in's time and scanner), not_confirmed, revoked,
// wrong_event and not_found. wrong_event and not_found never carry the
// attendee: another event's attendee does not leak (04 §E.5). Every scan
// is audited (ADR-0013), never with the token.

// CheckinResultKind is the result of a scan (04 §E.5).
type CheckinResultKind string

// Scan results.
const (
	ResultOK               CheckinResultKind = "ok"
	ResultAlreadyCheckedIn CheckinResultKind = "already_checked_in"
	ResultNotConfirmed     CheckinResultKind = "not_confirmed"
	ResultRevoked          CheckinResultKind = "revoked"
	ResultWrongEvent       CheckinResultKind = "wrong_event"
	ResultNotFound         CheckinResultKind = "not_found"
)

// CheckinResults lists every result (OpenAPI enum).
func CheckinResults() []CheckinResultKind {
	return []CheckinResultKind{ResultOK, ResultAlreadyCheckedIn, ResultNotConfirmed, ResultRevoked, ResultWrongEvent, ResultNotFound}
}

// Check-in limits.
const (
	maxDeviceID   = 100
	minVoidReason = 5
	maxVoidReason = 300
	// scanFuture / scanPast bound scanned_at (the device clock) around the
	// server clock.
	scanFuture = 5 * time.Minute
	scanPast   = 24 * time.Hour
	// checkinAggType is the audit resource type and outbox aggregate.
	checkinAggType = "checkin"
)

// CheckinInput is the body of createCheckin (04 §2.6, §E.5).
type CheckinInput struct {
	Method CheckinMethod
	// Token is the scanned QR content "KORO1:<token>" (method qr). A
	// bearer credential: never logged, stored or echoed.
	Token string
	// ManualCode is the typed code (method manual_code; "7KQ2-M9TX" or
	// "7kq2m9tx" are read the same).
	ManualCode   string
	ClientScanID uuid.UUID
	ScannedAt    time.Time
	DeviceID     string
}

// CheckinResult is the answer of a scan; it survives a JSON round trip
// (the Idempotency-Key replays it).
type CheckinResult struct {
	Result    CheckinResultKind `json:"result"`
	CheckinID *uuid.UUID        `json:"checkin_id,omitempty"`
	// Attendee is "Ayşe K." (absent for wrong_event and not_found).
	Attendee    string           `json:"attendee,omitempty"`
	CheckedInAt *time.Time       `json:"checked_in_at,omitempty"`
	Previous    *PreviousCheckin `json:"previous,omitempty"`
}

// PreviousCheckin is the first valid check-in of an already_checked_in
// scan.
type PreviousCheckin struct {
	CheckedInAt  time.Time `json:"checked_in_at"`
	StaffDisplay string    `json:"staff_display"`
}

// checkinEvent reads the event of a check-in action and authorises act on
// it: the event is the resource (03 §3.5; the caller's staff role in
// Attrs), so an invisible event is 404 and a visible one without the role
// 403 before anything else is read.
func (s *service) checkinEvent(ctx context.Context, a authz.Actor, act authz.Action, eventID uuid.UUID) (Event, bool, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return Event{}, false, err
	}
	sens, err := s.sensitive(ctx, e)
	if err != nil {
		return Event{}, false, err
	}
	role, err := s.staffRole(ctx, a, e)
	if err != nil {
		return Event{}, false, err
	}
	if err := authz.Can(ctx, a, act, eventResource(e, sens, role)); err != nil {
		return Event{}, false, err
	}
	return e, sens, nil
}

// CreateCheckin implements Service. Order: the event (404) → authz
// (404/403) → the body (422) → in one transaction with the
// Idempotency-Key: the organiser's status (403), the event's check-in
// state (409), then the chain above.
func (s *service) CreateCheckin(ctx context.Context, a authz.Actor, eventID uuid.UUID, in CheckinInput) (CheckinResult, error) {
	e, sens, err := s.checkinEvent(ctx, a, authz.EventsCheckinCreate, eventID)
	if err != nil {
		return CheckinResult{}, err
	}
	in, err = s.cleanCheckin(in)
	if err != nil {
		return CheckinResult{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
		In    CheckinInput
	}{"checkin", e.ID, in})
	if err != nil {
		return CheckinResult{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (CheckinResult, error) {
		return s.checkIn(ctx, a, e, sens, in)
	})
	return out, err
}

// cleanCheckin checks the body (422): the method's credential, the scan id,
// scanned_at near the server clock and the device id.
func (s *service) cleanCheckin(in CheckinInput) (CheckinInput, error) {
	switch in.Method {
	case MethodQR:
		if in.Token == "" || in.ManualCode != "" {
			return in, fieldErr("body.token", "method qr needs token and no manual_code")
		}
	case MethodManualCode:
		if in.ManualCode == "" || in.Token != "" {
			return in, fieldErr("body.manual_code", "method manual_code needs manual_code and no token")
		}
	default:
		return in, fieldErr("body.method", "must be qr or manual_code")
	}
	if in.ClientScanID == uuid.Nil() {
		return in, fieldErr("body.client_scan_id", "must be a UUID")
	}
	now := s.now()
	if in.ScannedAt.IsZero() || in.ScannedAt.After(now.Add(scanFuture)) || in.ScannedAt.Before(now.Add(-scanPast)) {
		return in, fieldErr("body.scanned_at", "must be within the last 24 hours (device clock)")
	}
	in.ScannedAt = in.ScannedAt.UTC().Truncate(time.Microsecond)
	if in.DeviceID != "" {
		d, err := cleanText("body.device_id", in.DeviceID, 1, maxDeviceID)
		if err != nil {
			return in, err
		}
		in.DeviceID = d
	}
	return in, nil
}

// checkinOpen: check-in is on, the event published (or completed) and its
// attendance not finalised (409 events.invalid_state otherwise).
func checkinOpen(e Event) error {
	switch {
	case !e.CheckinEnabled, e.AttendanceFinalizedAt != nil,
		e.Status != StatusPublished && e.Status != StatusCompleted:
		return ErrInvalidState
	}
	return nil
}

// checkIn is the transaction of CreateCheckin.
func (s *service) checkIn(ctx context.Context, a authz.Actor, e Event, sens bool, in CheckinInput) (CheckinResult, error) {
	cur, err := s.d.Store.KeyShareLockEvent(ctx, e.OrgID, e.ID)
	if err != nil {
		return CheckinResult{}, err
	}
	if err := checkinOpen(cur); err != nil {
		return CheckinResult{}, err
	}
	o, err := s.org(ctx, cur.OrgID)
	if err != nil {
		return CheckinResult{}, err
	}
	if err := writable(a, o); err != nil {
		return CheckinResult{}, err
	}
	// A client retry (same scan id, staff member and event) is the first
	// answer, whatever happened since.
	if prev, ok, err := s.d.Store.CheckinByClientScan(ctx, cur.OrgID, cur.ID, a.UserID, in.ClientScanID); err != nil {
		return CheckinResult{}, err
	} else if ok {
		return s.okResult(ctx, prev)
	}
	t, res, err := s.scannedTicket(ctx, cur, in)
	if err != nil {
		return CheckinResult{}, err
	}
	if res != "" {
		return s.scanOutcome(ctx, a, cur, in, CheckinResult{Result: res}, nil)
	}
	r, err := s.d.Store.GetRegistration(ctx, cur.OrgID, cur.ID, t.RegistrationID)
	if errors.Is(err, ErrNotFound) {
		return s.scanOutcome(ctx, a, cur, in, CheckinResult{Result: ResultNotFound}, nil)
	}
	if err != nil {
		return CheckinResult{}, err
	}
	if t.Status != TicketActive {
		return s.scanOutcome(ctx, a, cur, in, CheckinResult{Result: ResultRevoked, Attendee: s.attendee(ctx, r.UserID)}, &r)
	}
	c := Checkin{
		ID: uuid.NewV7(), EventID: cur.ID, OrgID: cur.OrgID, RegistrationID: r.ID, TicketID: t.ID, StaffUserID: a.UserID,
		Method: in.Method, ClientScanID: in.ClientScanID, ScannedAt: in.ScannedAt, ReceivedAt: s.now(),
	}
	if in.DeviceID != "" {
		c.DeviceID = &in.DeviceID
	}
	var (
		stored Checkin
		ok     bool
	)
	if r.Status == RegConfirmed {
		if stored, ok, err = s.d.Store.CheckIn(ctx, cur.OrgID, c); err != nil {
			return CheckinResult{}, err
		}
	}
	if !ok {
		return s.notCheckedIn(ctx, a, cur, in, r.ID)
	}
	if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsCheckinCreate, checkinAggType, stored.ID, cur.OrgID, "",
		map[string]any{"event_id": cur.ID.String(), "registration_id": r.ID.String(), "result": string(ResultOK),
			"method": string(in.Method)})); err != nil {
		return CheckinResult{}, err
	}
	env, err := domainevent.New(domainevent.CheckinRecorded{CheckinID: stored.ID, RegistrationID: r.ID, EventID: cur.ID,
		UserID: r.UserID, StaffUserID: a.UserID, Sensitive: sens}, checkinAggType, stored.ID)
	if err != nil {
		return CheckinResult{}, err
	}
	if err := s.d.Outbox.Add(ctx, env); err != nil {
		return CheckinResult{}, err
	}
	return s.okResult(ctx, stored)
}

// notCheckedIn is the outcome of a registration that did not move to
// checked_in: a concurrent scan of the same client_scan_id won (its
// answer), another scan won (already_checked_in), or the registration has
// no seat (not_confirmed).
func (s *service) notCheckedIn(ctx context.Context, a authz.Actor, e Event, in CheckinInput, regID uuid.UUID) (CheckinResult, error) {
	if prev, ok, err := s.d.Store.CheckinByClientScan(ctx, e.OrgID, e.ID, a.UserID, in.ClientScanID); err != nil {
		return CheckinResult{}, err
	} else if ok {
		return s.okResult(ctx, prev)
	}
	r, err := s.d.Store.GetRegistration(ctx, e.OrgID, e.ID, regID)
	if errors.Is(err, ErrNotFound) {
		return s.scanOutcome(ctx, a, e, in, CheckinResult{Result: ResultNotFound}, nil)
	}
	if err != nil {
		return CheckinResult{}, err
	}
	res := CheckinResult{Result: ResultNotConfirmed, Attendee: s.attendee(ctx, r.UserID)}
	if r.Status == RegCheckedIn {
		res.Result = ResultAlreadyCheckedIn
		first, ok, err := s.d.Store.ActiveCheckin(ctx, e.OrgID, e.ID, r.ID)
		if err != nil {
			return CheckinResult{}, err
		}
		if ok {
			res.Previous = &PreviousCheckin{CheckedInAt: first.ScannedAt, StaffDisplay: s.attendee(ctx, first.StaffUserID)}
		}
	}
	return s.scanOutcome(ctx, a, e, in, res, &r)
}

// scannedTicket resolves the presented credential to a ticket of e, or to
// the result of a scan that found none: not_found (unknown, forged or of a
// retired key) or wrong_event (a genuine ticket of another event; nothing
// of it is read further).
func (s *service) scannedTicket(ctx context.Context, e Event, in CheckinInput) (Ticket, CheckinResultKind, error) {
	if in.Method == MethodManualCode {
		code, ok := normalizeManualCode(in.ManualCode)
		if !ok {
			return Ticket{}, ResultNotFound, nil
		}
		t, ok, err := s.d.Store.TicketByManualCode(ctx, e.OrgID, e.ID, code)
		if err != nil || !ok {
			return Ticket{}, ResultNotFound, err
		}
		return t, "", nil
	}
	token, ok := parseQRPayload(in.Token)
	if !ok {
		return Ticket{}, ResultNotFound, nil
	}
	t, ok, err := s.d.Store.TicketByLookup(ctx, TicketLookup(token))
	if err != nil || !ok {
		return Ticket{}, ResultNotFound, err
	}
	switch err := s.verifyTicket(t, token); {
	case errors.Is(err, errTicketMAC), errors.Is(err, errTicketKey):
		return Ticket{}, ResultNotFound, nil
	case err != nil:
		return Ticket{}, "", err
	}
	if t.EventID != e.ID {
		return Ticket{}, ResultWrongEvent, nil
	}
	return t, "", nil
}

// scanOutcome audits a scan that checked nobody in and returns its result.
func (s *service) scanOutcome(ctx context.Context, a authz.Actor, e Event, in CheckinInput, res CheckinResult, r *Registration) (CheckinResult, error) {
	meta := map[string]any{"event_id": e.ID.String(), "result": string(res.Result), "method": string(in.Method)}
	if r != nil {
		meta["registration_id"] = r.ID.String()
	}
	if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsCheckinCreate, eventAggType, e.ID, e.OrgID, "", meta)); err != nil {
		return CheckinResult{}, err
	}
	return res, nil
}

// okResult is the answer of a recorded check-in (also its client retry).
func (s *service) okResult(ctx context.Context, c Checkin) (CheckinResult, error) {
	r, err := s.d.Store.GetRegistration(ctx, c.OrgID, c.EventID, c.RegistrationID)
	if err != nil {
		return CheckinResult{}, err
	}
	id, at := c.ID, c.ScannedAt.UTC()
	return CheckinResult{Result: ResultOK, CheckinID: &id, Attendee: s.attendee(ctx, r.UserID), CheckedInAt: &at}, nil
}

// attendee is "Ayşe K." (first name and the initial of the last name; 02
// §11.2, 04 §E.5); "" when the account is unknown or identity not wired.
func (s *service) attendee(ctx context.Context, userID uuid.UUID) string {
	if s.d.Users == nil {
		return ""
	}
	u, err := s.d.Users.Get(ctx, userID)
	if err != nil {
		return ""
	}
	return displayName(u.FirstName, u.LastName)
}

// displayName is "First L." (first name and last-name initial).
func displayName(first, last string) string {
	if i := lastInitial(last); i != "" {
		if first == "" {
			return i
		}
		return first + " " + i
	}
	return first
}

// CheckinVoidView is the answer of voidCheckin.
type CheckinVoidView struct {
	Checkin            Checkin
	RegistrationStatus RegStatus
}

// VoidCheckin implements Service: a manager (stf Em, the organiser's
// managers, k-po) voids a check-in with a reason; the registration is
// confirmed again (its seat kept). Order: the event (404) → authz → the
// reason (422) → in one transaction: the organiser's status (403), the
// attendance still open (409), the check-in of this event (404; voided:
// 409) → audit → outbox checkin.voided.
func (s *service) VoidCheckin(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, reason string) (CheckinVoidView, error) {
	e, _, err := s.checkinEvent(ctx, a, authz.EventsCheckinVoid, eventID)
	if err != nil {
		return CheckinVoidView{}, err
	}
	why, err := cleanText("body.void_reason", reason, minVoidReason, maxVoidReason)
	if err != nil {
		return CheckinVoidView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Event  uuid.UUID
		ID     uuid.UUID
		Reason string
	}{"void_checkin", e.ID, id, why})
	if err != nil {
		return CheckinVoidView{}, err
	}
	type voided struct {
		CheckinID uuid.UUID `json:"checkin_id"`
		VoidedAt  time.Time `json:"voided_at"`
		Status    RegStatus `json:"status"`
	}
	v, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (voided, error) {
		cur, err := s.d.Store.KeyShareLockEvent(ctx, e.OrgID, e.ID)
		if err != nil {
			return voided{}, err
		}
		if err := s.orgWritable(ctx, a, cur.OrgID); err != nil {
			return voided{}, err
		}
		if cur.AttendanceFinalizedAt != nil {
			return voided{}, ErrInvalidState
		}
		c, r, err := s.d.Store.VoidCheckin(ctx, cur.OrgID, cur.ID, id, a.UserID, why, s.now())
		if err != nil {
			return voided{}, err
		}
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsCheckinVoid, checkinAggType, c.ID, cur.OrgID, why,
			map[string]any{"event_id": cur.ID.String(), "registration_id": r.ID.String()})); err != nil {
			return voided{}, err
		}
		env, err := domainevent.New(domainevent.CheckinVoided{CheckinID: c.ID, RegistrationID: r.ID, UserID: r.UserID},
			checkinAggType, c.ID)
		if err != nil {
			return voided{}, err
		}
		if err := s.d.Outbox.Add(ctx, env); err != nil {
			return voided{}, err
		}
		return voided{CheckinID: c.ID, VoidedAt: *c.VoidedAt, Status: r.Status}, nil
	})
	if err != nil {
		return CheckinVoidView{}, err
	}
	at := v.VoidedAt
	return CheckinVoidView{Checkin: Checkin{ID: v.CheckinID, VoidedAt: &at}, RegistrationStatus: v.Status}, nil
}

// orgWritable is the organiser's status rule of writes (03 §2.3).
func (s *service) orgWritable(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return err
	}
	return writable(a, o)
}

// CheckinView is one row of listCheckins.
type CheckinView struct {
	Checkin  Checkin
	Attendee string // "Ayşe K."
}

// CheckinPage is a page of check-ins; Next is the keyset position of the
// next page (nil: last page).
type CheckinPage struct {
	Items []CheckinView
	Next  *After
}

// ListCheckins implements Service (events.checkin.list): the latest scan
// first; attendees as "Ayşe K." only (02 §11.2).
func (s *service) ListCheckins(ctx context.Context, a authz.Actor, eventID uuid.UUID, limit int, after *After) (CheckinPage, error) {
	e, _, err := s.checkinEvent(ctx, a, authz.EventsCheckinList, eventID)
	if err != nil {
		return CheckinPage{}, err
	}
	limit = page.ClampLimit(limit)
	rows, err := s.d.Store.ListCheckins(ctx, e.OrgID, e.ID, after, limit+1)
	if err != nil {
		return CheckinPage{}, err
	}
	out := CheckinPage{Items: []CheckinView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.ScannedAt, ID: last.ID}
	}
	names, err := s.registrantNames(ctx, e, rows)
	if err != nil {
		return CheckinPage{}, err
	}
	for _, c := range rows {
		out.Items = append(out.Items, CheckinView{Checkin: c, Attendee: names[c.RegistrationID]})
	}
	return out, nil
}

// registrantNames maps the registrations of rows to "Ayşe K.".
func (s *service) registrantNames(ctx context.Context, e Event, rows []Checkin) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(rows) == 0 || s.d.Users == nil {
		return out, nil
	}
	users := map[uuid.UUID]uuid.UUID{} // registration → user
	var ids []uuid.UUID
	for _, c := range rows {
		if _, seen := users[c.RegistrationID]; seen {
			continue
		}
		r, err := s.d.Store.GetRegistration(ctx, e.OrgID, e.ID, c.RegistrationID)
		if err != nil {
			return nil, err
		}
		users[c.RegistrationID] = r.UserID
		ids = append(ids, r.UserID)
	}
	us, err := s.d.Users.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for reg, uid := range users {
		if u, ok := us[uid]; ok {
			out[reg] = displayName(u.FirstName, u.LastName)
		}
	}
	return out, nil
}

// CheckinStats are the counters of getCheckinStats.
type CheckinStats struct {
	CheckinCounts
	UpdatedAt time.Time
}

// GetCheckinStats implements Service (events.checkin.stats; the live
// board is BE-054).
func (s *service) GetCheckinStats(ctx context.Context, a authz.Actor, eventID uuid.UUID) (CheckinStats, error) {
	e, _, err := s.checkinEvent(ctx, a, authz.EventsCheckinStats, eventID)
	if err != nil {
		return CheckinStats{}, err
	}
	n, err := s.d.Store.CheckinCounts(ctx, e.OrgID, e.ID)
	if err != nil {
		return CheckinStats{}, err
	}
	return CheckinStats{CheckinCounts: n, UpdatedAt: s.now()}, nil
}
