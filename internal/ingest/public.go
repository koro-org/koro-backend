package ingest

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// catalog is the resource of the public operations.
var catalog = authz.Resource{Type: authz.TypeIngestCatalog}

// adminSurface is the resource of the admin operations.
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// resourceImportedEvent is the audit resource type of an imported event.
const resourceImportedEvent = "imported_event"

// EventQuery narrows the public list and the club's suggestions.
type EventQuery struct {
	UniversityID *uuid.UUID
	ClubOrgID    *uuid.UUID
	From, To     *time.Time
	Limit        int
	After        *EventAfter
}

// ClubRef names the club of an imported event.
type ClubRef struct {
	ID   uuid.UUID
	Name string
	Slug string
}

// EventView is an imported event as the operations show it.
type EventView struct {
	Event      ImportedEvent
	Club       *ClubRef
	SourceHost string
}

// EventPage is a page of imported events.
type EventPage struct {
	Items []EventView
	Next  *EventAfter
}

// RemovalReason is the reason of an anonymous removal request.
type RemovalReason string

// Removal reasons.
const (
	RemovalPersonalData RemovalReason = "personal_data"
	RemovalIncorrect    RemovalReason = "incorrect"
	RemovalCopyright    RemovalReason = "copyright"
	RemovalCancelled    RemovalReason = "cancelled"
	RemovalOther        RemovalReason = "other"
)

// RemovalReasons lists the accepted reasons (OpenAPI enum).
func RemovalReasons() []RemovalReason {
	return []RemovalReason{RemovalPersonalData, RemovalIncorrect, RemovalCopyright, RemovalCancelled, RemovalOther}
}

// ListImportedEvents implements Service: the upcoming imported events
// (status imported, not ended), by start.
func (s *service) ListImportedEvents(ctx context.Context, a authz.Actor, q EventQuery) (EventPage, error) {
	if err := authz.Can(ctx, a, authz.IngestImportedEventList, catalog); err != nil {
		return EventPage{}, err
	}
	return s.listEvents(ctx, EventFilter{
		Statuses: []Status{StatusImported}, UniversityID: q.UniversityID, ClubOrgID: q.ClubOrgID, From: q.From, To: q.To,
	}, q, true)
}

// listEvents reads one page of f (upcoming only when upcoming).
func (s *service) listEvents(ctx context.Context, f EventFilter, q EventQuery, upcoming bool) (EventPage, error) {
	if q.From != nil && q.To != nil && !q.To.After(*q.From) {
		return EventPage{}, problem.Field("query.to", "must be after from")
	}
	if upcoming {
		now := s.now()
		f.EndsAfter = &now
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListEvents(ctx, f, q.After, limit+1)
	if err != nil {
		return EventPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	views, err := s.views(ctx, rows)
	if err != nil {
		return EventPage{}, err
	}
	out := EventPage{Items: views}
	if more {
		last := rows[len(rows)-1]
		out.Next = &EventAfter{StartsAt: last.StartsAt, ID: last.ID}
	}
	return out, nil
}

// views adds the club names (one read) and the source hosts.
func (s *service) views(ctx context.Context, rows []ImportedEvent) ([]EventView, error) {
	var ids []uuid.UUID
	for _, r := range rows {
		if r.ClubOrgID != nil && !slices.Contains(ids, *r.ClubOrgID) {
			ids = append(ids, *r.ClubOrgID)
		}
	}
	names, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]EventView, len(rows))
	for i, r := range rows {
		out[i] = EventView{Event: r, SourceHost: hostOf(r.SourceURL)}
		if r.ClubOrgID != nil {
			if o, ok := names[*r.ClubOrgID]; ok {
				out[i].Club = &ClubRef{ID: o.ID, Name: o.Name, Slug: o.Slug}
			}
		}
	}
	return out, nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// publicEvent reads an event the public surface shows (imported, not
// ended); anything else is 404.
func (s *service) publicEvent(ctx context.Context, id uuid.UUID) (ImportedEvent, error) {
	e, err := s.d.Store.GetEvent(ctx, id)
	switch {
	case errors.Is(err, errNotFound):
		return ImportedEvent{}, ErrNotFound
	case err != nil:
		return ImportedEvent{}, err
	}
	if e.Status != StatusImported || !e.End().After(s.now()) {
		return ImportedEvent{}, ErrNotFound
	}
	return e, nil
}

// GetImportedEvent implements Service.
func (s *service) GetImportedEvent(ctx context.Context, a authz.Actor, id uuid.UUID) (EventView, error) {
	if err := authz.Can(ctx, a, authz.IngestImportedEventRead, catalog); err != nil {
		return EventView{}, err
	}
	e, err := s.publicEvent(ctx, id)
	if err != nil {
		return EventView{}, err
	}
	v, err := s.views(ctx, []ImportedEvent{e})
	if err != nil {
		return EventView{}, err
	}
	return v[0], nil
}

// RequestRemoval implements Service: anyone (an account is not needed:
// the person a page names may have none) asks that a public imported
// event be removed. It is hidden at once (removal_request) and waits in
// the admins' list (adminListImportedEvents?status=hidden): the data is
// unverified, so the request wins until an admin restores it. The reason
// is audited, never a name or an address (the body has no free text).
func (s *service) RequestRemoval(ctx context.Context, a authz.Actor, id uuid.UUID, reason RemovalReason) error {
	if err := authz.Can(ctx, a, authz.IngestImportedEventRequestRemoval, catalog); err != nil {
		return err
	}
	if !slices.Contains(RemovalReasons(), reason) {
		return problem.Field("body.reason", "unknown reason")
	}
	if _, err := s.publicEvent(ctx, id); err != nil {
		return err
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.d.Store.LockEvent(ctx, id)
		if errors.Is(err, errNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.Status != StatusImported {
			return nil // hidden meanwhile: the request is already met
		}
		hr := HiddenByRequest
		if err := s.d.Store.SetEventStatus(ctx, id, StatusHidden, &hr, nil); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.IngestImportedEventRequestRemoval), Outcome: audit.Success, ResourceType: resourceImportedEvent,
			ResourceID: &id, OrgID: e.ClubOrgID, Metadata: map[string]any{"reason": string(reason)},
		}.By(a))
	})
	if err != nil {
		return err
	}
	s.m.removalReq.Add(1)
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "imported event hidden on a removal request",
		slog.String("imported_event_id", id.String()), slog.String("reason", string(reason)))
	return nil
}
