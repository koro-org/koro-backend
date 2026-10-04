package events

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"time"
	"uuid"
)

// The in-memory registration store (BE-028). It enforces the CHECKs of
// event_registrations and registration_tickets (02 §11.6–11.7), the
// partial unique index of live registrations, the ticket uniques and the
// capacity rule; ClaimSeat is atomic under the store mutex like the
// conditional UPDATE under the row lock. Locks are no-ops (no
// transactions; db.NopTx).

var manualCodeRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{8}$`)

// checkRegistration applies the CHECKs of event_registrations.
func checkRegistration(r Registration) error {
	switch {
	case !r.Status.Valid(), !r.Source.Valid(), runes(r.DecisionNote) > 500, runes(r.CancelReason) > 300,
		r.CancelledByKind != nil && *r.CancelledByKind != ByUser && *r.CancelledByKind != ByOrg && *r.CancelledByKind != BySystem,
		r.AnswersPurgeAt.IsZero():
		return errCheck
	case r.AnswersSensitiveEnc != nil && r.SensitivePurgeAt == nil,
		r.Status == RegWaitlisted && r.WaitlistedAt == nil,
		(r.Status == RegConfirmed || r.Status == RegCheckedIn || r.Status == RegNoShow) && r.ConfirmedAt == nil,
		(r.Status == RegCancelled || r.Status == RegWithdrawn) && (r.CancelledAt == nil || r.CancelledByKind == nil),
		r.Status == RegCheckedIn && r.CheckedInAt == nil,
		r.AnswersPurgedAt != nil && (string(jsonOr(r.Answers)) != "{}" || r.AnswersSensitiveEnc != nil):
		return errCheck
	}
	return nil
}

func cloneReg(r Registration) Registration {
	r.Answers, r.ConsentSnapshot = cloneJSON(r.Answers), cloneJSON(r.ConsentSnapshot)
	r.AnswersSensitiveEnc = slices.Clone(r.AnswersSensitiveEnc)
	return r
}

// ClaimSeat implements Store.
func (s *MemStore) ClaimSeat(_ context.Context, orgID, eventID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[eventID]
	if !ok || e.OrgID != orgID || e.Status != StatusPublished || e.Capacity != nil && e.ConfirmedCount >= *e.Capacity {
		return false, nil
	}
	e.ConfirmedCount++
	e.UpdatedAt = s.stamp(e.UpdatedAt) // the touch trigger
	s.events[eventID] = e
	return true, nil
}

// ReleaseSeat implements Store.
func (s *MemStore) ReleaseSeat(_ context.Context, orgID, eventID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[eventID]
	if !ok || e.OrgID != orgID || e.ConfirmedCount == 0 {
		return nil
	}
	e.ConfirmedCount--
	e.UpdatedAt = s.stamp(e.UpdatedAt)
	s.events[eventID] = e
	return nil
}

// ShareLockEvent implements Store.
func (s *MemStore) ShareLockEvent(ctx context.Context, orgID, eventID uuid.UUID) (Event, error) {
	e, err := s.GetEvent(ctx, eventID)
	if err != nil || e.OrgID != orgID {
		return Event{}, ErrNotFound
	}
	return e, nil
}

// GetEvents implements Store.
func (s *MemStore) GetEvents(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]Event{}
	for _, id := range ids {
		if e, ok := s.events[id]; ok {
			e.DescriptionJSON = cloneJSON(e.DescriptionJSON)
			out[id] = e
		}
	}
	return out, nil
}

// InsertRegistration implements Store.
func (s *MemStore) InsertRegistration(_ context.Context, r Registration) (Registration, error) {
	if r.ID == uuid.Nil() || r.UserID == uuid.Nil() {
		return Registration{}, errCheck
	}
	r.Answers, r.ConsentSnapshot = jsonOr(r.Answers), jsonOr(r.ConsentSnapshot)
	if err := checkRegistration(r); err != nil {
		return Registration{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[r.EventID]
	if !ok || e.OrgID != r.OrgID {
		return Registration{}, ErrNotFound
	}
	if _, dup := s.regs[r.ID]; dup {
		return Registration{}, ErrAlreadyRegistered
	}
	if r.Status.Active() && s.activeOf(r.EventID, r.UserID) != nil {
		return Registration{}, ErrAlreadyRegistered // event_registrations_active_uq
	}
	r.OrgID = e.OrgID // 06 H-38
	r.CreatedAt = s.stamp(time.Time{})
	r.UpdatedAt = r.CreatedAt
	s.regs[r.ID] = cloneReg(r)
	return cloneReg(r), nil
}

// activeOf returns the live registration of a user for an event (nil: none).
func (s *MemStore) activeOf(eventID, userID uuid.UUID) *Registration {
	for _, x := range s.regs {
		if x.EventID == eventID && x.UserID == userID && x.Status.Active() {
			return &x
		}
	}
	return nil
}

// GetRegistration implements Store.
func (s *MemStore) GetRegistration(_ context.Context, orgID, eventID, id uuid.UUID) (Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regs[id]
	if !ok || r.OrgID != orgID || r.EventID != eventID {
		return Registration{}, ErrNotFound
	}
	return cloneReg(r), nil
}

// LockRegistration implements Store.
func (s *MemStore) LockRegistration(ctx context.Context, orgID, eventID, id uuid.UUID) (Registration, error) {
	return s.GetRegistration(ctx, orgID, eventID, id)
}

// GetUserRegistration implements Store.
func (s *MemStore) GetUserRegistration(_ context.Context, userID, id uuid.UUID) (Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regs[id]
	if !ok || r.UserID != userID {
		return Registration{}, ErrNotFound
	}
	return cloneReg(r), nil
}

// ActiveRegistration implements Store.
func (s *MemStore) ActiveRegistration(_ context.Context, orgID, eventID, userID uuid.UUID) (Registration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.activeOf(eventID, userID)
	if r == nil || r.OrgID != orgID {
		return Registration{}, false, nil
	}
	return cloneReg(*r), true, nil
}

// UpdateRegistration implements Store.
func (s *MemStore) UpdateRegistration(_ context.Context, r Registration) (Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.regs[r.ID]
	if !ok || cur.OrgID != r.OrgID || cur.EventID != r.EventID {
		return Registration{}, ErrNotFound
	}
	next := cur
	next.Status, next.WaitlistedAt, next.ConfirmedAt = r.Status, r.WaitlistedAt, r.ConfirmedAt
	next.ApprovedBy, next.ApprovedAt, next.DecisionNote = r.ApprovedBy, r.ApprovedAt, r.DecisionNote
	next.CancelledAt, next.CancelledByKind, next.CancelReason = r.CancelledAt, r.CancelledByKind, r.CancelReason
	if err := checkRegistration(next); err != nil {
		return Registration{}, err
	}
	if next.Status.Active() && !cur.Status.Active() {
		if x := s.activeOf(next.EventID, next.UserID); x != nil && x.ID != next.ID {
			return Registration{}, ErrAlreadyRegistered
		}
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.regs[r.ID] = next
	return cloneReg(next), nil
}

// waitlistOf returns the waitlisted registrations of an event, FIFO.
func (s *MemStore) waitlistOf(orgID, eventID uuid.UUID) []Registration {
	var out []Registration
	for _, r := range s.regs {
		if r.EventID == eventID && r.OrgID == orgID && r.Status == RegWaitlisted {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Registration) int {
		return cmp.Or(a.WaitlistedAt.Compare(*b.WaitlistedAt), compareUUID(a.ID, b.ID))
	})
	return out
}

// NextWaitlisted implements Store.
func (s *MemStore) NextWaitlisted(_ context.Context, orgID, eventID uuid.UUID) (Registration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waitlistOf(orgID, eventID)
	if len(w) == 0 {
		return Registration{}, false, nil
	}
	return cloneReg(w[0]), true, nil
}

// WaitlistPosition implements Store.
func (s *MemStore) WaitlistPosition(_ context.Context, orgID, eventID, id uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.waitlistOf(orgID, eventID) {
		if r.ID == id {
			return i + 1, nil
		}
	}
	return 0, nil
}

func sortRegs(rs []Registration, desc bool) {
	slices.SortFunc(rs, func(a, b Registration) int {
		c := cmp.Or(a.CreatedAt.Compare(b.CreatedAt), compareUUID(a.ID, b.ID))
		if desc {
			return -c
		}
		return c
	})
}

// ListEventRegistrations implements Store.
func (s *MemStore) ListEventRegistrations(_ context.Context, orgID, eventID uuid.UUID, f RegFilter, after *After, limit int) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Registration{}
	for _, r := range s.regs {
		if r.OrgID != orgID || r.EventID != eventID || len(f.Statuses) > 0 && !slices.Contains(f.Statuses, r.Status) {
			continue
		}
		if after != nil {
			c := cmp.Or(r.CreatedAt.Compare(after.At), compareUUID(r.ID, after.ID))
			if f.Desc && c >= 0 || !f.Desc && c <= 0 {
				continue
			}
		}
		out = append(out, cloneReg(r))
	}
	sortRegs(out, f.Desc)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListUserRegistrations implements Store.
func (s *MemStore) ListUserRegistrations(_ context.Context, userID uuid.UUID, f MyRegFilter, after *After, limit int) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type row struct {
		r     Registration
		start time.Time
	}
	var rows []row
	for _, r := range s.regs {
		e, ok := s.events[r.EventID]
		switch {
		case r.UserID != userID, !ok,
			len(f.Statuses) > 0 && !slices.Contains(f.Statuses, r.Status),
			f.EndsAfter != nil && !e.EndsAt.After(*f.EndsAfter),
			after != nil && cmp.Or(e.StartsAt.Compare(after.At), compareUUID(r.ID, after.ID)) <= 0:
			continue
		}
		rows = append(rows, row{cloneReg(r), e.StartsAt})
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Or(a.start.Compare(b.start), compareUUID(a.r.ID, b.r.ID)) })
	out := []Registration{}
	for _, x := range rows {
		if len(out) == limit {
			break
		}
		out = append(out, x.r)
	}
	return out, nil
}

// LockActiveRegistrations implements Store.
func (s *MemStore) LockActiveRegistrations(_ context.Context, orgID, eventID uuid.UUID, limit int) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Registration{}
	for _, r := range s.regs {
		if r.OrgID == orgID && r.EventID == eventID && (r.Status == RegPending || r.Status == RegConfirmed || r.Status == RegWaitlisted) {
			out = append(out, cloneReg(r))
		}
	}
	sortRegs(out, false)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ExpireWaitlisted implements Store.
func (s *MemStore) ExpireWaitlisted(_ context.Context, now time.Time, limit int) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Registration
	for _, r := range s.regs {
		if e, ok := s.events[r.EventID]; ok && r.Status == RegWaitlisted && !e.StartsAt.After(now) {
			due = append(due, r)
		}
	}
	slices.SortFunc(due, func(a, b Registration) int {
		return cmp.Or(a.WaitlistedAt.Compare(*b.WaitlistedAt), compareUUID(a.ID, b.ID))
	})
	out := []Registration{}
	for _, r := range due {
		if len(out) == limit {
			break
		}
		r.Status, r.UpdatedAt = RegExpired, s.stamp(r.UpdatedAt)
		s.regs[r.ID] = r
		out = append(out, cloneReg(r))
	}
	return out, nil
}

// seatsOf counts the confirmed and checked-in registrations of an event.
func (s *MemStore) seatsOf(eventID uuid.UUID) int {
	n := 0
	for _, r := range s.regs {
		if r.EventID == eventID && r.Status.Counted() {
			n++
		}
	}
	return n
}

// SeatCounts implements Store.
func (s *MemStore) SeatCounts(_ context.Context, endsAfter time.Time, after uuid.UUID, limit int) ([]SeatCount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var evs []Event
	for _, e := range s.events {
		if e.Status != StatusDraft && e.EndsAt.After(endsAfter) && compareUUID(e.ID, after) > 0 {
			evs = append(evs, e)
		}
	}
	slices.SortFunc(evs, func(a, b Event) int { return compareUUID(a.ID, b.ID) })
	out := []SeatCount{}
	for _, e := range evs {
		if len(out) == limit {
			break
		}
		out = append(out, SeatCount{EventID: e.ID, OrgID: e.OrgID, Stored: e.ConfirmedCount, Actual: s.seatsOf(e.ID)})
	}
	return out, nil
}

// CountSeats implements Store.
func (s *MemStore) CountSeats(_ context.Context, orgID, eventID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.events[eventID]; !ok || e.OrgID != orgID {
		return 0, nil
	}
	return s.seatsOf(eventID), nil
}

// SetConfirmedCount implements Store.
func (s *MemStore) SetConfirmedCount(_ context.Context, orgID, eventID uuid.UUID, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[eventID]
	if !ok || e.OrgID != orgID {
		return ErrNotFound
	}
	e.ConfirmedCount = n
	if err := checkEvent(e); err != nil {
		return ErrCapacityFull // events_capacity_fill_chk
	}
	e.UpdatedAt = s.stamp(e.UpdatedAt)
	s.events[eventID] = e
	return nil
}

// ShiftPurgeDates implements Store.
func (s *MemStore) ShiftPurgeDates(_ context.Context, orgID, eventID uuid.UUID, answersAt, sensitiveAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.regs {
		if r.OrgID != orgID || r.EventID != eventID || r.AnswersPurgedAt != nil {
			continue
		}
		r.AnswersPurgeAt = answersAt
		if r.SensitivePurgeAt != nil {
			r.SensitivePurgeAt = &sensitiveAt
		}
		s.regs[id] = r
	}
	return nil
}

// CountUserRegistrationsInOrg implements Store.
func (s *MemStore) CountUserRegistrationsInOrg(_ context.Context, orgID, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.regs {
		if r.OrgID == orgID && r.UserID == userID && r.Status.Active() {
			n++
		}
	}
	return n, nil
}

// checkTicket applies the CHECKs of registration_tickets.
func checkTicket(t Ticket) error {
	revoked := t.Status == TicketRevoked
	switch {
	case t.Version <= 0, t.KeyID == "", len(t.TokenLookup) != 32, !manualCodeRe.MatchString(t.ManualCode),
		t.Status != TicketActive && !revoked, revoked != (t.RevokedAt != nil), revoked != (t.RevokeReason != nil):
		return errCheck
	}
	return nil
}

// InsertTicket implements Store.
func (s *MemStore) InsertTicket(_ context.Context, orgID uuid.UUID, t Ticket) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regs[t.RegistrationID]
	if !ok || r.OrgID != orgID {
		return Ticket{}, false, ErrNotFound
	}
	t.EventID, t.Status, t.RevokedAt, t.RevokeReason = r.EventID, TicketActive, nil, nil // 06 H-38
	if err := checkTicket(t); err != nil {
		return Ticket{}, false, err
	}
	for _, x := range s.tickets {
		switch {
		case x.EventID == t.EventID && x.ManualCode == t.ManualCode:
			return Ticket{}, false, nil // ON CONFLICT (event_id, manual_code) DO NOTHING
		case x.ID == t.ID, string(x.TokenLookup) == string(t.TokenLookup),
			x.RegistrationID == t.RegistrationID && (x.Version == t.Version || x.Status == TicketActive):
			return Ticket{}, false, errCheck // the other uniques: 409/422 in PostgreSQL
		}
	}
	if t.IssuedAt.IsZero() {
		t.IssuedAt = s.stamp(time.Time{})
	}
	t.TokenLookup = slices.Clone(t.TokenLookup)
	s.tickets = append(s.tickets, t)
	return t, true, nil
}

// ActiveTicket implements Store.
func (s *MemStore) ActiveTicket(_ context.Context, orgID, registrationID uuid.UUID) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.regs[registrationID]; !ok || r.OrgID != orgID {
		return Ticket{}, false, nil
	}
	for _, t := range s.tickets {
		if t.RegistrationID == registrationID && t.Status == TicketActive {
			return t, true, nil
		}
	}
	return Ticket{}, false, nil
}

// MaxTicketVersion implements Store.
func (s *MemStore) MaxTicketVersion(_ context.Context, orgID, registrationID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.regs[registrationID]; !ok || r.OrgID != orgID {
		return 0, nil
	}
	n := 0
	for _, t := range s.tickets {
		if t.RegistrationID == registrationID && t.Version > n {
			n = t.Version
		}
	}
	return n, nil
}

// RevokeTickets implements Store.
func (s *MemStore) RevokeTickets(_ context.Context, orgID, registrationID uuid.UUID, reason RevokeReason, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.regs[registrationID]; !ok || r.OrgID != orgID {
		return 0, nil
	}
	n := 0
	for i, t := range s.tickets {
		if t.RegistrationID == registrationID && t.Status == TicketActive {
			why := reason
			s.tickets[i].Status, s.tickets[i].RevokedAt, s.tickets[i].RevokeReason = TicketRevoked, &at, &why
			n++
		}
	}
	return n, nil
}

// RegistrationsOfUser implements Store.
func (s *MemStore) RegistrationsOfUser(_ context.Context, userID uuid.UUID) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Registration{}
	for _, r := range s.regs {
		if r.UserID == userID {
			out = append(out, cloneReg(r))
		}
	}
	sortRegs(out, false)
	return out, nil
}

// TicketsOfRegistration implements Store.
func (s *MemStore) TicketsOfRegistration(_ context.Context, orgID, registrationID uuid.UUID) ([]Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Ticket{}
	if r, ok := s.regs[registrationID]; !ok || r.OrgID != orgID {
		return out, nil
	}
	for _, t := range s.tickets {
		if t.RegistrationID == registrationID {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Ticket) int { return cmp.Compare(a.Version, b.Version) })
	return out, nil
}

// DeleteRegistration implements Store.
func (s *MemStore) DeleteRegistration(_ context.Context, orgID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regs[id]
	if !ok || r.OrgID != orgID {
		return ErrNotFound
	}
	delete(s.regs, id)
	s.tickets = slices.DeleteFunc(s.tickets, func(t Ticket) bool { return t.RegistrationID == id })
	return nil
}

// PutRegistration stores r as is (matrix fixtures and tests): a counted
// registration takes its seat without moving the event's updated_at (the
// fixtures' ETags stay constant). The CHECKs and the live-registration
// unique still apply.
func (s *MemStore) PutRegistration(r Registration) error {
	r.Answers, r.ConsentSnapshot = jsonOr(r.Answers), jsonOr(r.ConsentSnapshot)
	if err := checkRegistration(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[r.EventID]
	if !ok || e.OrgID != r.OrgID {
		return ErrNotFound
	}
	if r.Status.Active() && s.activeOf(r.EventID, r.UserID) != nil {
		return ErrAlreadyRegistered
	}
	if r.Status.Counted() {
		e.ConfirmedCount++
		if err := checkEvent(e); err != nil {
			return ErrCapacityFull
		}
		s.events[r.EventID] = e
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = s.stamp(time.Time{})
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	s.regs[r.ID] = cloneReg(r)
	return nil
}

// Tickets returns the tickets of a registration (tests).
func (s *MemStore) Tickets(registrationID uuid.UUID) []Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Ticket
	for _, t := range s.tickets {
		if t.RegistrationID == registrationID {
			out = append(out, t)
		}
	}
	return out
}

// SetSeatCount stores n as an event's confirmed_count as is (tests of the
// reconciliation: a drift the conditional UPDATE never makes).
func (s *MemStore) SetSeatCount(eventID uuid.UUID, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.events[eventID]
	e.ConfirmedCount = n
	s.events[eventID] = e
}
