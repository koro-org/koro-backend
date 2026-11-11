package ingest

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// The platform admin's operations (admin surface): the sources (list,
// read with the latest runs, create, the built-in pilot set, enable /
// disable / interval, run now), the imported events (list, hide / restore,
// delete) and the imported clubs waiting for a category review.

// resourceSource is the audit resource type of a source.
const resourceSource = "ingest_source"

// maxRuns is how many runs adminGetIngestSource shows.
const maxRuns = 20

// SourceQuery narrows adminListIngestSources.
type SourceQuery struct {
	Filter SourceFilter
	Limit  int
	After  *uuid.UUID
}

// SourcePage is a page of sources.
type SourcePage struct {
	Items []Source
	Next  *uuid.UUID
}

// SourceView is a source with its latest runs (newest first).
type SourceView struct {
	Source Source
	Runs   []Run
}

// SourceInput is the body of adminCreateIngestSource.
type SourceInput struct {
	Kind         Kind
	Adapter      AdapterName
	URL          string
	Label        string
	UniversityID uuid.UUID
	ClubOrgID    *uuid.UUID
	Interval     time.Duration // 0: the kind's default
	Enabled      *bool         // nil: the environment's default
}

// SourcePatch is the body of adminUpdateIngestSource; nil leaves a field.
type SourcePatch struct {
	Enabled  *bool
	Interval *time.Duration
}

// Interval bounds (ingest_sources_interval_chk).
const (
	minInterval = time.Hour
	maxInterval = 30 * 24 * time.Hour
)

// AdminListSources implements Service.
func (s *service) AdminListSources(ctx context.Context, a authz.Actor, q SourceQuery) (SourcePage, error) {
	if err := authz.Can(ctx, a, authz.IngestSourceList, adminSurface); err != nil {
		return SourcePage{}, err
	}
	if q.Filter.Kind != "" && !q.Filter.Kind.Valid() {
		return SourcePage{}, problem.Field("query.kind", "unknown kind")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListSources(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return SourcePage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := SourcePage{Items: rows}
	if more {
		id := rows[len(rows)-1].ID
		out.Next = &id
	}
	return out, nil
}

// AdminGetSource implements Service.
func (s *service) AdminGetSource(ctx context.Context, a authz.Actor, id uuid.UUID) (SourceView, error) {
	if err := authz.Can(ctx, a, authz.IngestSourceRead, adminSurface); err != nil {
		return SourceView{}, err
	}
	return s.sourceView(ctx, id)
}

func (s *service) sourceView(ctx context.Context, id uuid.UUID) (SourceView, error) {
	src, err := s.d.Store.GetSource(ctx, id)
	switch {
	case errors.Is(err, errNotFound):
		return SourceView{}, ErrNotFound
	case err != nil:
		return SourceView{}, err
	}
	runs, err := s.d.Store.ListRuns(ctx, id, maxRuns)
	if err != nil {
		return SourceView{}, err
	}
	return SourceView{Source: src, Runs: runs}, nil
}

// AdminCreateSource implements Service: the URL must be one the guard
// fetches (422 ingest.url_not_allowed), the adapter must yield the kind,
// the university must exist and a club must be of that university.
func (s *service) AdminCreateSource(ctx context.Context, a authz.Actor, in SourceInput) (SourceView, error) {
	if err := authz.Can(ctx, a, authz.IngestSourceCreate, adminSurface); err != nil {
		return SourceView{}, err
	}
	src, err := s.checkSource(ctx, in)
	if err != nil {
		return SourceView{}, err
	}
	var id uuid.UUID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		row, created, err := s.d.Store.InsertSource(ctx, src)
		if err != nil {
			return err
		}
		if !created {
			return ErrSourceExists
		}
		id = row.ID
		return s.auditSource(ctx, a, authz.IngestSourceCreate, row, nil)
	})
	if err != nil {
		return SourceView{}, err
	}
	return s.sourceView(ctx, id)
}

// checkSource validates an input and builds its row.
func (s *service) checkSource(ctx context.Context, in SourceInput) (Source, error) {
	u, host, err := ParseSourceURL(in.URL)
	if err != nil {
		return Source{}, ErrURLNotAllowed.WithFields(ErrURLNotAllowed.At("body.url"))
	}
	label := clip(trimLine(in.Label), 120)
	switch {
	case !in.Kind.Valid():
		return Source{}, problem.Field("body.kind", "unknown kind")
	case !in.Adapter.Valid():
		return Source{}, problem.Field("body.adapter", "unknown adapter")
	case label == "":
		return Source{}, problem.Field("body.label", "is required")
	case in.Interval != 0 && (in.Interval < minInterval || in.Interval > maxInterval):
		return Source{}, problem.Field("body.interval_hours", "must be between 1 and 720 hours")
	}
	adapter := s.d.Adapters[in.Adapter]
	if adapter == nil || adapter.Kind() != in.Kind || in.Kind == KindClubList && in.ClubOrgID != nil {
		return Source{}, ErrAdapterMismatch
	}
	if adapter.Accept() == nil && in.Enabled != nil && *in.Enabled {
		return Source{}, ErrNotFetchable
	}
	if _, err := s.d.Directory.GetUniversity(ctx, in.UniversityID); err != nil {
		if errors.Is(err, directory.ErrNotFound) {
			return Source{}, problem.Field("body.university_id", "unknown university")
		}
		return Source{}, err
	}
	if in.ClubOrgID != nil {
		c, err := s.d.Directory.GetClub(ctx, *in.ClubOrgID)
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return Source{}, problem.Field("body.club_id", "unknown club")
		case err != nil:
			return Source{}, err
		case c.UniversityID != in.UniversityID:
			return Source{}, ErrAdapterMismatch
		}
	}
	in.URL, in.Label = u, label
	src := s.newSource(in, host)
	if adapter.Accept() == nil {
		src.Enabled, src.NextRunAt = false, nil
	}
	return src, nil
}

// AdminUpdateSource implements Service: enabling clears the pause and
// makes the source due now; disabling records paused_reason admin.
func (s *service) AdminUpdateSource(ctx context.Context, a authz.Actor, id uuid.UUID, p SourcePatch) (SourceView, error) {
	if err := authz.Can(ctx, a, authz.IngestSourceUpdate, adminSurface); err != nil {
		return SourceView{}, err
	}
	if p.Enabled == nil && p.Interval == nil {
		return SourceView{}, problem.Field("body", "nothing to update")
	}
	if p.Interval != nil && (*p.Interval < minInterval || *p.Interval > maxInterval) {
		return SourceView{}, problem.Field("body.interval_hours", "must be between 1 and 720 hours")
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		src, err := s.d.Store.LockSource(ctx, id)
		if errors.Is(err, errNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		before := src
		if p.Interval != nil && src.IntervalSeconds != 0 { // a one-off page keeps 0
			src.IntervalSeconds = int(*p.Interval / time.Second)
		}
		if p.Enabled != nil && *p.Enabled != src.Enabled {
			if *p.Enabled {
				if a := s.d.Adapters[src.Adapter]; a == nil || a.Accept() == nil {
					return ErrNotFetchable
				}
				now := s.now()
				src.Enabled, src.PausedReason, src.FailCount, src.NextRunAt = true, nil, 0, &now
			} else {
				reason := PausedAdmin
				src.Enabled, src.PausedReason = false, &reason
			}
		}
		if err := s.d.Store.UpdateSource(ctx, src); err != nil {
			return err
		}
		return s.auditSource(ctx, a, authz.IngestSourceUpdate, src, map[string]any{
			"enabled_before": before.Enabled, "interval_seconds_before": before.IntervalSeconds,
		})
	})
	if err != nil {
		return SourceView{}, err
	}
	return s.sourceView(ctx, id)
}

// AdminRunSource implements Service: an enabled source becomes due now
// (the next fetch_due tick runs it, under the host gap and robots.txt).
func (s *service) AdminRunSource(ctx context.Context, a authz.Actor, id uuid.UUID) (SourceView, error) {
	if err := authz.Can(ctx, a, authz.IngestSourceRun, adminSurface); err != nil {
		return SourceView{}, err
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		src, err := s.d.Store.LockSource(ctx, id)
		if errors.Is(err, errNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !src.Enabled {
			return ErrInvalidState.WithFields(problem.FieldError{Location: "source", Message: "is disabled"})
		}
		now := s.now()
		src.NextRunAt = &now
		if err := s.d.Store.UpdateSource(ctx, src); err != nil {
			return err
		}
		return s.auditSource(ctx, a, authz.IngestSourceRun, src, nil)
	})
	if err != nil {
		return SourceView{}, err
	}
	return s.sourceView(ctx, id)
}

// auditSource writes the audit record of an admin write on a source (IDs,
// adapter and host only).
func (s *service) auditSource(ctx context.Context, a authz.Actor, act authz.Action, src Source, extra map[string]any) error {
	meta := map[string]any{"adapter": string(src.Adapter), "host": src.Host, "enabled": src.Enabled,
		"university_id": src.UniversityID.String()}
	for k, v := range extra {
		meta[k] = v
	}
	id := src.ID
	return s.d.Audit.Log(ctx, audit.Event{
		Action: string(act), Outcome: audit.Success, ResourceType: resourceSource, ResourceID: &id, OrgID: src.ClubOrgID,
		Metadata: meta,
	}.By(a))
}

// AdminEventQuery narrows adminListImportedEvents.
type AdminEventQuery struct {
	Filter EventFilter
	Limit  int
	After  *EventAfter
}

// AdminListImportedEvents implements Service: every status (filter), past
// ones included.
func (s *service) AdminListImportedEvents(ctx context.Context, a authz.Actor, q AdminEventQuery) (EventPage, error) {
	if err := authz.Can(ctx, a, authz.IngestImportedEventListAdmin, adminSurface); err != nil {
		return EventPage{}, err
	}
	for _, st := range q.Filter.Statuses {
		if !st.Valid() {
			return EventPage{}, problem.Field("query.status", "unknown status")
		}
	}
	return s.listEvents(ctx, q.Filter, EventQuery{From: q.Filter.From, To: q.Filter.To, Limit: q.Limit, After: q.After}, false)
}

// AdminSetEventStatus implements Service: hidden (admin) or imported
// (restores a hidden event, a removal request's included). A promoted or
// rejected event is the club's decision: 409.
func (s *service) AdminSetEventStatus(ctx context.Context, a authz.Actor, id uuid.UUID, st Status) (EventView, error) {
	if err := authz.Can(ctx, a, authz.IngestImportedEventModerate, adminSurface); err != nil {
		return EventView{}, err
	}
	if st != StatusHidden && st != StatusImported {
		return EventView{}, problem.Field("body.status", "must be hidden or imported")
	}
	var out ImportedEvent
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.d.Store.LockEvent(ctx, id)
		if errors.Is(err, errNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.Status != StatusImported && e.Status != StatusHidden {
			return ErrInvalidState
		}
		var reason *HiddenReason
		if st == StatusHidden {
			reason = ptr(HiddenByAdmin)
			if e.HiddenReason != nil {
				reason = e.HiddenReason // keep the reason of the first hide
			}
		}
		if err := s.d.Store.SetEventStatus(ctx, id, st, reason, nil); err != nil {
			return err
		}
		e.Status, e.HiddenReason = st, reason
		out = e
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.IngestImportedEventModerate), Outcome: audit.Success, ResourceType: resourceImportedEvent,
			ResourceID: &id, OrgID: e.ClubOrgID, Metadata: map[string]any{"status": string(st)},
		}.By(a))
	})
	if err != nil {
		return EventView{}, err
	}
	v, err := s.views(ctx, []ImportedEvent{out})
	if err != nil {
		return EventView{}, err
	}
	return v[0], nil
}

// AdminDeleteImportedEvent implements Service (hard delete; audited).
func (s *service) AdminDeleteImportedEvent(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.IngestImportedEventDelete, adminSurface); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.d.Store.LockEvent(ctx, id)
		if errors.Is(err, errNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := s.d.Store.DeleteEvent(ctx, id); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.IngestImportedEventDelete), Outcome: audit.Success, ResourceType: resourceImportedEvent,
			ResourceID: &id, OrgID: e.ClubOrgID, Metadata: map[string]any{"status": string(e.Status)},
		}.By(a))
	})
}

// ClubQuery narrows adminListImportedClubs.
type ClubQuery struct {
	Filter ImportedClubFilter
	Limit  int
	After  *uuid.UUID
}

// ClubPage is a page of imported clubs with their names.
type ClubPage struct {
	Items []ClubItem
	Next  *uuid.UUID
}

// ClubItem is an imported club with its org's name and slug.
type ClubItem struct {
	Club ImportedClub
	Name string
	Slug string
}

// AdminListImportedClubs implements Service.
func (s *service) AdminListImportedClubs(ctx context.Context, a authz.Actor, q ClubQuery) (ClubPage, error) {
	if err := authz.Can(ctx, a, authz.IngestImportedClubList, adminSurface); err != nil {
		return ClubPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListImportedClubs(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return ClubPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ClubOrgID
	}
	names, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return ClubPage{}, err
	}
	out := ClubPage{Items: make([]ClubItem, len(rows))}
	for i, r := range rows {
		out.Items[i] = ClubItem{Club: r, Name: names[r.ClubOrgID].Name, Slug: names[r.ClubOrgID].Slug}
	}
	if more {
		id := rows[len(rows)-1].ClubOrgID
		out.Next = &id
	}
	return out, nil
}

// AdminMarkClubReviewed implements Service: the platform admin checked
// the club's category and sensitivity (directory's own operations).
func (s *service) AdminMarkClubReviewed(ctx context.Context, a authz.Actor, clubOrgID uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.IngestImportedClubReview, adminSurface); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.MarkClubReviewed(ctx, clubOrgID); err != nil {
			if errors.Is(err, errNotFound) {
				return ErrNotFound
			}
			return err
		}
		id := clubOrgID
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.IngestImportedClubReview), Outcome: audit.Success, ResourceType: authz.TypeClub,
			ResourceID: &id, OrgID: &id,
		}.By(a))
	})
}

// trimLine collapses the spaces of a one-line text.
func trimLine(s string) string {
	out := []rune{}
	space := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			space = len(out) > 0
			continue
		}
		if space {
			out = append(out, ' ')
			space = false
		}
		out = append(out, r)
	}
	return string(out)
}
