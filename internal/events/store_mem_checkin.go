package events

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// The in-memory check-in store (BE-031). It enforces the CHECKs of
// event_checkins (02 §11.8), the global unique client_scan_id and the
// partial unique index of valid check-ins; CheckIn and VoidCheckin are
// atomic under the store mutex like the conditional UPDATEs under the row
// locks of PostgreSQL.

// checkCheckin applies the CHECKs of event_checkins.
func checkCheckin(c Checkin) error {
	switch {
	case c.ID == uuid.Nil(), c.ClientScanID == uuid.Nil(), !c.Method.Valid(), runes(c.DeviceID) > 100, c.ScannedAt.IsZero(),
		(c.VoidedAt == nil) != (c.VoidedBy == nil), (c.VoidedAt == nil) != (c.VoidReason == nil),
		c.VoidReason != nil && (runes(c.VoidReason) < 5 || runes(c.VoidReason) > 300):
		return errCheck
	}
	return nil
}

// KeyShareLockEvent implements Store.
func (s *MemStore) KeyShareLockEvent(ctx context.Context, orgID, eventID uuid.UUID) (Event, error) {
	return s.ShareLockEvent(ctx, orgID, eventID)
}

// TicketByLookup implements Store.
func (s *MemStore) TicketByLookup(_ context.Context, lookup []byte) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tickets {
		if bytes.Equal(t.TokenLookup, lookup) {
			return t, true, nil
		}
	}
	return Ticket{}, false, nil
}

// TicketByManualCode implements Store.
func (s *MemStore) TicketByManualCode(_ context.Context, orgID, eventID uuid.UUID, code string) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tickets {
		if r, ok := s.regs[t.RegistrationID]; ok && r.OrgID == orgID && t.EventID == eventID && t.ManualCode == code {
			return t, true, nil
		}
	}
	return Ticket{}, false, nil
}

// GetTicket implements Store.
func (s *MemStore) GetTicket(_ context.Context, id uuid.UUID) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tickets {
		if t.ID == id {
			return t, nil
		}
	}
	return Ticket{}, ErrNotFound
}

// CheckinByClientScan implements Store.
func (s *MemStore) CheckinByClientScan(_ context.Context, orgID, eventID, staffUserID, clientScanID uuid.UUID) (Checkin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checkins {
		if c.ClientScanID == clientScanID && c.EventID == eventID && c.StaffUserID == staffUserID && c.OrgID == orgID {
			return c, true, nil
		}
	}
	return Checkin{}, false, nil
}

// CheckIn implements Store.
func (s *MemStore) CheckIn(_ context.Context, orgID uuid.UUID, c Checkin) (Checkin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.checkins, func(x Checkin) bool { return x.ClientScanID == c.ClientScanID }) {
		return Checkin{}, false, ErrClientScanTaken // event_checkins_client_scan_id_uq
	}
	r, ok := s.regs[c.RegistrationID]
	if !ok || r.OrgID != orgID || r.EventID != c.EventID || r.Status != RegConfirmed {
		return Checkin{}, false, nil
	}
	c.EventID, c.OrgID = r.EventID, r.OrgID // 06 H-38
	c.VoidedAt, c.VoidedBy, c.VoidReason = nil, nil, nil
	if err := checkCheckin(c); err != nil {
		return Checkin{}, false, err
	}
	for _, x := range s.checkins {
		switch {
		case x.ID == c.ID:
			return Checkin{}, false, errCheck
		case x.RegistrationID == c.RegistrationID && x.VoidedAt == nil:
			return Checkin{}, false, problem.ErrConflict // event_checkins_one_active_uq
		}
	}
	at := c.ScannedAt
	r.Status, r.CheckedInAt, r.UpdatedAt = RegCheckedIn, &at, s.stamp(r.UpdatedAt)
	s.regs[r.ID] = r
	if c.ReceivedAt.IsZero() {
		c.ReceivedAt = s.stamp(time.Time{})
	}
	s.checkins = append(s.checkins, c)
	return c, true, nil
}

// ActiveCheckin implements Store.
func (s *MemStore) ActiveCheckin(_ context.Context, orgID, eventID, registrationID uuid.UUID) (Checkin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checkins {
		if c.RegistrationID == registrationID && c.EventID == eventID && c.OrgID == orgID && c.VoidedAt == nil {
			return c, true, nil
		}
	}
	return Checkin{}, false, nil
}

// VoidCheckin implements Store.
func (s *MemStore) VoidCheckin(_ context.Context, orgID, eventID, id, by uuid.UUID, reason string, at time.Time) (Checkin, Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.checkins, func(c Checkin) bool { return c.ID == id && c.EventID == eventID && c.OrgID == orgID })
	if i < 0 {
		return Checkin{}, Registration{}, ErrNotFound
	}
	c := s.checkins[i]
	r, ok := s.regs[c.RegistrationID]
	if c.VoidedAt != nil || !ok || r.Status != RegCheckedIn {
		return Checkin{}, Registration{}, ErrInvalidState
	}
	c.VoidedAt, c.VoidedBy, c.VoidReason = &at, &by, &reason
	if err := checkCheckin(c); err != nil {
		return Checkin{}, Registration{}, err
	}
	r.Status, r.CheckedInAt, r.UpdatedAt = RegConfirmed, nil, s.stamp(r.UpdatedAt)
	s.regs[r.ID] = r
	s.checkins[i] = c
	return c, cloneReg(r), nil
}

// ListCheckins implements Store.
func (s *MemStore) ListCheckins(_ context.Context, orgID, eventID uuid.UUID, after *After, limit int) ([]Checkin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Checkin{}
	for _, c := range s.checkins {
		if c.OrgID != orgID || c.EventID != eventID {
			continue
		}
		if after != nil && cmp.Or(c.ScannedAt.Compare(after.At), compareUUID(c.ID, after.ID)) >= 0 {
			continue
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Checkin) int {
		return -cmp.Or(a.ScannedAt.Compare(b.ScannedAt), compareUUID(a.ID, b.ID))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CheckinCounts implements Store.
func (s *MemStore) CheckinCounts(_ context.Context, orgID, eventID uuid.UUID) (CheckinCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n CheckinCounts
	for _, r := range s.regs {
		if r.OrgID != orgID || r.EventID != eventID {
			continue
		}
		switch {
		case r.Status.Counted():
			n.Confirmed++
			if r.Status == RegCheckedIn {
				n.CheckedIn++
			}
		case r.Status == RegWaitlisted:
			n.Waitlisted++
		case r.Status == RegNoShow:
			n.NoShow++
		}
	}
	for _, c := range s.checkins {
		if c.OrgID == orgID && c.EventID == eventID && c.VoidedAt != nil {
			n.Voided++
		}
	}
	return n, nil
}

// FinalizeAttendance implements Store.
func (s *MemStore) FinalizeAttendance(_ context.Context, orgID, eventID uuid.UUID, by *uuid.UUID, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[eventID]
	if !ok || e.OrgID != orgID || e.AttendanceFinalizedAt != nil {
		return false, nil
	}
	e.AttendanceFinalizedAt, e.AttendanceFinalizedBy = &at, by
	if err := checkEvent(e); err != nil {
		return false, err
	}
	e.UpdatedAt = s.stamp(e.UpdatedAt)
	s.events[eventID] = e
	return true, nil
}

// MarkNoShows implements Store.
func (s *MemStore) MarkNoShows(_ context.Context, orgID, eventID uuid.UUID, limit int) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Registration
	for _, r := range s.regs {
		if r.OrgID == orgID && r.EventID == eventID && r.Status == RegConfirmed {
			due = append(due, r)
		}
	}
	sortRegs(due, false)
	out := []Registration{}
	for _, r := range due {
		if len(out) == limit {
			break
		}
		r.Status, r.UpdatedAt = RegNoShow, s.stamp(r.UpdatedAt)
		s.regs[r.ID] = r
		out = append(out, cloneReg(r))
	}
	if e, ok := s.events[eventID]; ok && len(out) > 0 {
		e.ConfirmedCount = max(e.ConfirmedCount-len(out), 0)
		e.UpdatedAt = s.stamp(e.UpdatedAt)
		s.events[eventID] = e
	}
	return out, nil
}

// DueAttendance implements Store.
func (s *MemStore) DueAttendance(_ context.Context, endedBefore time.Time, after uuid.UUID, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	checked := map[uuid.UUID]bool{}
	for _, c := range s.checkins {
		if c.VoidedAt == nil {
			checked[c.EventID] = true
		}
	}
	out := []Event{}
	for _, e := range s.events {
		if e.CheckinEnabled && e.AttendanceFinalizedAt == nil && (e.Status == StatusPublished || e.Status == StatusCompleted) &&
			!e.EndsAt.After(endedBefore) && compareUUID(e.ID, after) > 0 && checked[e.ID] {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b Event) int { return compareUUID(a.ID, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SeatHolders implements Store.
func (s *MemStore) SeatHolders(_ context.Context, orgID, eventID, after uuid.UUID, limit int) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Registration{}
	for _, r := range s.regs {
		if r.OrgID == orgID && r.EventID == eventID && r.Status.Counted() && compareUUID(r.ID, after) > 0 {
			out = append(out, cloneReg(r))
		}
	}
	slices.SortFunc(out, func(a, b Registration) int { return compareUUID(a.ID, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sortCheckins(cs []Checkin) {
	slices.SortFunc(cs, func(a, b Checkin) int { return cmp.Or(a.ScannedAt.Compare(b.ScannedAt), compareUUID(a.ID, b.ID)) })
}

// CheckinsOfRegistration implements Store.
func (s *MemStore) CheckinsOfRegistration(_ context.Context, orgID, registrationID uuid.UUID) ([]Checkin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Checkin{}
	for _, c := range s.checkins {
		if c.OrgID == orgID && c.RegistrationID == registrationID {
			out = append(out, c)
		}
	}
	sortCheckins(out)
	return out, nil
}

// CheckinsByStaff implements Store.
func (s *MemStore) CheckinsByStaff(_ context.Context, userID uuid.UUID) ([]Checkin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Checkin{}
	for _, c := range s.checkins {
		if c.StaffUserID == userID {
			out = append(out, c)
		}
	}
	sortCheckins(out)
	return out, nil
}

// PutTicket stores t as is (matrix fixtures and tests): the CHECKs and the
// uniques of registration_tickets still apply; event_id comes from the
// registration (06 H-38).
func (s *MemStore) PutTicket(t Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regs[t.RegistrationID]
	if !ok {
		return ErrNotFound
	}
	t.EventID = r.EventID
	if err := checkTicket(t); err != nil {
		return err
	}
	for _, x := range s.tickets {
		if x.ID == t.ID || bytes.Equal(x.TokenLookup, t.TokenLookup) || x.EventID == t.EventID && x.ManualCode == t.ManualCode ||
			x.RegistrationID == t.RegistrationID && (x.Version == t.Version || x.Status == TicketActive && t.Status == TicketActive) {
			return errCheck
		}
	}
	t.TokenLookup = slices.Clone(t.TokenLookup)
	s.tickets = append(s.tickets, t)
	return nil
}

// Checkins returns the check-ins of a registration (tests).
func (s *MemStore) Checkins(registrationID uuid.UUID) []Checkin {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Checkin
	for _, c := range s.checkins {
		if c.RegistrationID == registrationID {
			out = append(out, c)
		}
	}
	return out
}

// RewriteTicketLookup overwrites the token_lookup of a ticket (tests of a
// tampered row: a database write without the key).
func (s *MemStore) RewriteTicketLookup(id uuid.UUID, lookup []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tickets {
		if t.ID == id {
			s.tickets[i].TokenLookup = slices.Clone(lookup)
			return nil
		}
	}
	return ErrNotFound
}

// PutCheckin stores c as is (matrix fixtures): its registration must be
// checked in; event_id and org_id come from the registration (06 H-38).
func (s *MemStore) PutCheckin(c Checkin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regs[c.RegistrationID]
	if !ok {
		return ErrNotFound
	}
	c.EventID, c.OrgID = r.EventID, r.OrgID
	if err := checkCheckin(c); err != nil {
		return err
	}
	if r.Status != RegCheckedIn || slices.ContainsFunc(s.checkins, func(x Checkin) bool {
		return x.ID == c.ID || x.ClientScanID == c.ClientScanID || x.RegistrationID == c.RegistrationID && x.VoidedAt == nil
	}) {
		return errCheck
	}
	s.checkins = append(s.checkins, c)
	return nil
}
