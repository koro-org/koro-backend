package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"

	"koro/internal/events"
)

// matrixEventStart is the start of every matrix event: after the matrix
// clock (2026-10-03), so a draft fixture can be published.
var matrixEventStart = time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

// matrixFormV1 is the published version 1 of a fixture form; matrixFormDraft
// its draft (one more, optional field): publishEventForm publishes it as
// version 2 (BE-026).
var (
	matrixFormV1 = events.FormSchema{SchemaVersion: events.FormSchemaVersion, Fields: []events.FormField{
		{Key: "motivation", Type: events.FieldLongText, Label: events.LocalText{"tr": "Neden katılmak istiyorsun?"}, Required: true},
	}}
	matrixFormDraft = events.FormSchema{SchemaVersion: events.FormSchemaVersion, Fields: []events.FormField{
		matrixFormV1.Fields[0],
		{Key: "school", Type: events.FieldShortText, Label: events.LocalText{"tr": "Okulun"}},
	}}
)

// matrixFixtures seeds the events of the matrix (03 §7.3; fixtures/
// events.yaml): kind event (org, status, visibility, university, form:
// published gives the form a published version and a newer draft,
// checkin, capacity, waitlist, mode, ended: before the matrix clock), kind
// registration (event, persona user, status; BE-028; ticket: the manual
// code of its active ticket, BE-031), kind staff (event, persona or helper
// user, role) and kind checkin (registration, staff; BE-031). Every event is dated
// matrixOrgTime, so an untouched event's ETag is "1hmhuo3hji000" (the
// If-Match the matrix files send).
func (w *eventsWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	mem, ok := w.built.(*events.MemStore)
	if !ok {
		return fmt.Errorf("events: the matrix needs the in-memory store, got %T", w.built)
	}
	creator, err := fx.ID("club_admin")
	if err != nil {
		return err
	}
	fixtures := fx.Of("events")
	// Events first, then their staff, registrations and check-ins.
	rank := map[string]int{"event": 0, "staff": 1, "registration": 2, "checkin": 3}
	slices.SortStableFunc(fixtures, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	for _, f := range fixtures {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		switch f.Kind {
		case "event":
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			str := func(k string) string { s, _ := f.Fields[k].(string); return s }
			checkin, _ := f.Fields["checkin"].(bool)
			waitlist, _ := f.Fields["waitlist"].(bool)
			mode := events.ModeAuto
			if str("mode") != "" {
				mode = events.RegistrationMode(str("mode"))
			}
			t := matrixOrgTime
			starts := matrixEventStart
			if ended, _ := f.Fields["ended"].(bool); ended { // BE-031: ended before the matrix clock
				starts = matrixOrgTime.Add(10 * time.Hour)
			}
			e := events.Event{
				ID: id, OrgID: org, CreatedBy: creator, Title: "Matris " + f.Name, Slug: matrixSlug(f.Name),
				Category: events.CategoryWorkshop, Format: events.FormatInPerson, City: new("İstanbul"),
				StartsAt: starts, EndsAt: starts.Add(2 * time.Hour),
				Visibility: events.Visibility(str("visibility")), Status: events.Status(str("status")),
				RequiresRegistration: true, RegistrationMode: mode, CheckinEnabled: checkin, WaitlistEnabled: waitlist,
				CreatedAt: t, UpdatedAt: t,
			}
			if n, ok := f.Fields["capacity"].(int); ok {
				e.Capacity = &n
			}
			if e.Status != events.StatusDraft {
				e.PublishedAt = &t
			}
			if str("university") != "" {
				u, err := fx.Ref(f, "university")
				if err != nil {
					return err
				}
				e.VisibilityUniversityID = &u
			}
			if err := mem.PutEvent(e); err != nil {
				return fmt.Errorf("event %s: %w", f.Name, err)
			}
			if err := mem.InsertForm(ctx, org, events.Form{ID: uuid.NewV7(), EventID: id,
				DraftSchema: events.JSON(`{"schema_version":1,"fields":[]}`)}); err != nil {
				return fmt.Errorf("form of %s: %w", f.Name, err)
			}
			draft := events.FormSchema{SchemaVersion: events.FormSchemaVersion, Fields: []events.FormField{}}
			if str("form") == "published" {
				if err := mem.PutFormVersion(id, uuid.NewV7(), matrixFormV1); err != nil {
					return fmt.Errorf("form version of %s: %w", f.Name, err)
				}
				draft = matrixFormDraft
			}
			// The draft last, at the fixed instant: its ETag is the event's.
			if err := mem.PutFormDraft(id, draft, t); err != nil {
				return fmt.Errorf("form draft of %s: %w", f.Name, err)
			}
		case "staff":
			ev, err := fx.Ref(f, "event")
			if err != nil {
				return err
			}
			user, err := fx.Ref(f, "user")
			if err != nil {
				return err
			}
			e, err := mem.GetEvent(ctx, ev)
			if err != nil {
				return fmt.Errorf("staff %s: %w", f.Name, err)
			}
			role, _ := f.Fields["role"].(string)
			if _, err := mem.InsertStaff(ctx, events.Staff{EventID: ev, UserID: user, OrgID: e.OrgID,
				Role: events.StaffRole(role), InvitedBy: creator}); err != nil {
				return fmt.Errorf("staff %s: %w", f.Name, err)
			}
		case "registration":
			ev, err := fx.Ref(f, "event")
			if err != nil {
				return err
			}
			user, err := fx.Ref(f, "user")
			if err != nil {
				return err
			}
			e, err := mem.GetEvent(ctx, ev)
			if err != nil {
				return fmt.Errorf("registration %s: %w", f.Name, err)
			}
			st, _ := f.Fields["status"].(string)
			t := matrixOrgTime
			r := events.Registration{ID: id, EventID: ev, OrgID: e.OrgID, UserID: user, Status: events.RegStatus(st),
				Source: events.SourceWeb, Answers: events.JSON(`{"motivation":"Matris cevabı"}`),
				AnswersPurgeAt: e.EndsAt.Add(90 * 24 * time.Hour), CreatedAt: t, UpdatedAt: t}
			if v, ok := mem.Form(ev); ok && v.CurrentVersionID != nil {
				r.FormVersionID = v.CurrentVersionID
			}
			switch r.Status {
			case events.RegConfirmed:
				r.ConfirmedAt = &t
			case events.RegCheckedIn:
				r.ConfirmedAt, r.CheckedInAt = &t, &t
			case events.RegWaitlisted:
				r.WaitlistedAt = &t
			}
			if err := mem.PutRegistration(r); err != nil {
				return fmt.Errorf("registration %s: %w", f.Name, err)
			}
			// BE-031: ticket gives the registration its active ticket (version
			// 1, the field is its manual code; the token is derived from the
			// key ring of the build, like a real ticket).
			if code, _ := f.Fields["ticket"].(string); code != "" {
				key := w.keys.ActiveKeyID()
				token, err := events.TicketToken(w.keys, key, id, 1)
				if err != nil {
					return fmt.Errorf("ticket of %s: %w", f.Name, err)
				}
				if err := mem.PutTicket(events.Ticket{ID: uuid.NewV7(), RegistrationID: id, Version: 1, KeyID: key,
					TokenLookup: events.TicketLookup(token), ManualCode: code, Status: events.TicketActive, IssuedAt: t}); err != nil {
					return fmt.Errorf("ticket of %s: %w", f.Name, err)
				}
			}
		case "checkin":
			// BE-031: the valid check-in of a checked-in registration with a
			// ticket, scanned by the staff persona.
			reg, err := fx.Ref(f, "registration")
			if err != nil {
				return err
			}
			staff, err := fx.Ref(f, "staff")
			if err != nil {
				return err
			}
			tickets := mem.Tickets(reg)
			if len(tickets) == 0 {
				return fmt.Errorf("checkin %s: the registration has no ticket", f.Name)
			}
			t := matrixOrgTime
			if err := mem.PutCheckin(events.Checkin{ID: id, RegistrationID: reg, TicketID: tickets[0].ID, StaffUserID: staff,
				Method: events.MethodQR, ClientScanID: uuid.NewV7(), ScannedAt: t, ReceivedAt: t}); err != nil {
				return fmt.Errorf("checkin %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}
