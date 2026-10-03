package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"koro/internal/events"
)

// matrixEventStart is the start of every matrix event: after the matrix
// clock (2026-10-03), so a draft fixture can be published.
var matrixEventStart = time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

// matrixFixtures seeds the events of the matrix (03 §7.3; fixtures/
// events.yaml): kind event (org, status, visibility, university, form:
// published gives the form a published version, checkin) and kind staff
// (event, persona or helper user, role). Every event is dated
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
	// Events first, then their staff.
	slices.SortStableFunc(fixtures, func(a, b fixture) int { return strings.Compare(a.Kind, b.Kind) })
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
			t := matrixOrgTime
			e := events.Event{
				ID: id, OrgID: org, CreatedBy: creator, Title: "Matris " + f.Name, Slug: matrixSlug(f.Name),
				Category: events.CategoryWorkshop, Format: events.FormatInPerson, City: new("İstanbul"),
				StartsAt: matrixEventStart, EndsAt: matrixEventStart.Add(2 * time.Hour),
				Visibility: events.Visibility(str("visibility")), Status: events.Status(str("status")),
				RequiresRegistration: true, RegistrationMode: events.ModeAuto, CheckinEnabled: checkin,
				CreatedAt: t, UpdatedAt: t,
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
			if str("form") == "published" {
				if err := mem.SetFormVersion(id, uuid.NewV7()); err != nil {
					return fmt.Errorf("form version of %s: %w", f.Name, err)
				}
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
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}
