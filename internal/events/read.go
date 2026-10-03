package events

import (
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/page"
)

// ListQuery narrows listEvents (04 §2.6: q, org_id, university_id,
// category, format, city, starts_from, starts_to; sort starts_at or
// -published_at). Repeated org_id / university_id values are ORed; the two
// filters together intersect.
type ListQuery struct {
	Q             string
	OrgIDs        []uuid.UUID
	UniversityIDs []uuid.UUID
	Category      Category
	Format        Format
	City          string
	StartsFrom    *time.Time
	StartsTo      *time.Time
	Sort          string // SortStartsAt (default) or SortPublishedNew
	Limit         int
	After         *After
}

// OrgListQuery narrows listOrgEvents (status filter; sort starts_at).
type OrgListQuery struct {
	Statuses []Status
	Limit    int
	After    *After
}

// EventPage is a page of events. Next is the keyset position of the next
// page (nil: last page); Sort is the order the page was read in.
type EventPage struct {
	Items []EventView
	Next  *After
	Sort  string
}

// EventView is an event as one caller may see it. Manager is true when the
// caller manages the event (events.event.update): only then does the DTO
// carry online_url, the cover file ID and the sensitive category flag (03
// §5.6: online_url for approved attendees and managers; attendees: BE-028).
type EventView struct {
	Event    Event
	Org      orgs.OrgSummary
	ETag     string
	CoverURL string
	Manager  bool
}

// StaffView is one staff member as the staff list shows it (04 §2.6, §E.4):
// the first name and the initial of the last name only.
type StaffView struct {
	UserID      uuid.UUID
	FirstName   string
	LastInitial string
	Role        StaffRole
	CreatedAt   time.Time
}

// Get implements Events.
func (s *service) Get(ctx context.Context, eventID uuid.UUID) (EventSummary, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return EventSummary{}, err
	}
	sens, err := s.sensitive(ctx, e)
	if err != nil {
		return EventSummary{}, err
	}
	return EventSummary{
		ID: e.ID, OrgID: e.OrgID, Title: e.Title, Status: e.Status, StartsAt: e.StartsAt, EndsAt: e.EndsAt,
		Visibility: e.Visibility, Sensitive: sens,
	}, nil
}

// GetEvent implements Service: the event (404 when missing or not visible,
// 03 §3.6), its organiser and, for a manager, the full view.
func (s *service) GetEvent(ctx context.Context, a authz.Actor, id uuid.UUID) (EventView, error) {
	e, err := s.d.Store.GetEvent(ctx, id)
	if err != nil {
		return EventView{}, err
	}
	res, err := s.resource(ctx, a, e)
	if err != nil {
		return EventView{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsEventRead, res); err != nil {
		return EventView{}, err
	}
	o, err := s.org(ctx, e.OrgID)
	if err != nil {
		return EventView{}, err
	}
	manager := authz.Can(ctx, a, authz.EventsEventUpdate, res) == nil
	return s.view(ctx, e, o, manager), nil
}

// ListEvents implements Service. Only published events the caller may see
// are listed (03 §3.6); without a date range, events that have ended are
// left out.
func (s *service) ListEvents(ctx context.Context, a authz.Actor, q ListQuery) (EventPage, error) {
	if err := authz.Can(ctx, a, authz.EventsEventList, catalogResource); err != nil {
		return EventPage{}, err
	}
	text, err := cleanQuery("query.q", q.Q)
	if err != nil {
		return EventPage{}, err
	}
	f := ListFilter{
		Viewer: viewerOf(a), Q: text, Category: q.Category, Format: q.Format, StartsFrom: q.StartsFrom,
		StartsTo: q.StartsTo, Sort: q.Sort,
	}
	if q.City != "" {
		c, err := cleanText("query.city", q.City, 1, maxCity)
		if err != nil {
			return EventPage{}, err
		}
		f.City = c
	}
	if f.Sort == "" {
		f.Sort = SortStartsAt
	}
	if q.StartsFrom != nil && q.StartsTo != nil && !q.StartsTo.After(*q.StartsFrom) {
		return EventPage{}, fieldErr("query.starts_to", "must be after starts_from")
	}
	if q.StartsFrom == nil && q.StartsTo == nil {
		now := s.now()
		f.EndsAfter = &now
	}
	if f.OrgIDs, err = s.organisers(ctx, q.OrgIDs, q.UniversityIDs); err != nil {
		return EventPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListEvents(ctx, f, q.After, limit+1)
	if err != nil {
		return EventPage{}, err
	}
	return s.page(ctx, rows, limit, f.Sort, func(Event) bool { return false })
}

// organisers is the org filter of listEvents: nil when neither org_id nor
// university_id is given; the university filter is the university's own
// org and its clubs (directory).
func (s *service) organisers(ctx context.Context, orgIDs, uniIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(uniIDs) == 0 {
		if len(orgIDs) == 0 {
			return nil, nil
		}
		return slices.Clone(orgIDs), nil
	}
	ofUnis := []uuid.UUID{}
	for _, u := range uniIDs {
		ids, err := s.d.Catalog.UniversityOrgs(ctx, u)
		if err != nil {
			return nil, err
		}
		ofUnis = append(ofUnis, ids...)
	}
	if len(orgIDs) == 0 {
		return ofUnis, nil
	}
	out := []uuid.UUID{}
	for _, id := range orgIDs {
		if slices.Contains(ofUnis, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// viewerOf is what makes a non-public event visible to a (03 §3.6).
func viewerOf(a authz.Actor) Viewer {
	if !a.IsUser() {
		return Viewer{}
	}
	if a.HasAnyPlatformRole() {
		return Viewer{All: true}
	}
	v := Viewer{UserID: &a.UserID}
	if a.IsVerifiedStudent() && a.UniversityID != nil {
		u := *a.UniversityID
		v.UniversityID = &u
	}
	for _, m := range a.Memberships {
		if !slices.Contains(v.MemberOrgs, m.OrgID) {
			v.MemberOrgs = append(v.MemberOrgs, m.OrgID)
		}
	}
	return v
}

// ListOrgEvents implements Service: every event of the org, drafts
// included, for its management surface (03 §7.6: non-members 404).
func (s *service) ListOrgEvents(ctx context.Context, a authz.Actor, orgID uuid.UUID, q OrgListQuery) (EventPage, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return EventPage{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsEventListOrg, orgResource(o)); err != nil {
		return EventPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOrgEvents(ctx, o.ID, OrgListFilter{Statuses: q.Statuses}, q.After, limit+1)
	if err != nil {
		return EventPage{}, err
	}
	// The org list shows online_url to whoever may edit the event, not to
	// the moderator or support reading it (the grants do not depend on the
	// sensitive flag or a staff role).
	return s.page(ctx, rows, limit, SortStartsAt, func(e Event) bool {
		return authz.Can(ctx, a, authz.EventsEventUpdate, eventResource(e, false, "")) == nil
	})
}

// page cuts rows to limit, loads the organisers and builds the views;
// manager reports whether the caller manages an event of the page.
func (s *service) page(ctx context.Context, rows []Event, limit int, sort string, manager func(Event) bool) (EventPage, error) {
	out := EventPage{Items: []EventView{}, Sort: sort}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.StartsAt, ID: last.ID}
		if sort == SortPublishedNew && last.PublishedAt != nil {
			out.Next.At = *last.PublishedAt
		}
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, e := range rows {
		if !slices.Contains(ids, e.OrgID) {
			ids = append(ids, e.OrgID)
		}
	}
	os, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return EventPage{}, err
	}
	for _, e := range rows {
		o, ok := os[e.OrgID]
		if !ok {
			continue // an org row never disappears; a missing one is not shown
		}
		out.Items = append(out.Items, s.view(ctx, e, o, manager(e)))
	}
	return out, nil
}

// view builds the view of e: its ETag and a short-lived cover URL (a
// missing or unclean cover never fails a read).
func (s *service) view(ctx context.Context, e Event, o orgs.OrgSummary, manager bool) EventView {
	v := EventView{Event: e, Org: o, ETag: etagOf(e.UpdatedAt), Manager: manager}
	if e.CoverFileID != nil && s.d.Files != nil {
		if dl, err := s.d.Files.PresignGet(ctx, *e.CoverFileID, coverTTL); err == nil {
			v.CoverURL = dl.URL
		}
	}
	return v
}

// ListStaff implements Service (events.staff.list).
func (s *service) ListStaff(ctx context.Context, a authz.Actor, eventID uuid.UUID) ([]StaffView, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, authz.EventsStaffList, e); err != nil {
		return nil, err
	}
	rows, err := s.d.Store.ListStaff(ctx, e.OrgID, e.ID)
	if err != nil {
		return nil, err
	}
	return s.staffViews(ctx, rows)
}

// staffViews adds the names of the staff members (identity); a missing
// account shows no name.
func (s *service) staffViews(ctx context.Context, rows []Staff) ([]StaffView, error) {
	names := map[uuid.UUID]userName{}
	if s.d.Users != nil && len(rows) > 0 {
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.UserID
		}
		us, err := s.d.Users.GetMany(ctx, ids)
		if err != nil {
			return nil, err
		}
		for id, u := range us {
			names[id] = userName{first: u.FirstName, last: u.LastName}
		}
	}
	out := make([]StaffView, len(rows))
	for i, r := range rows {
		n := names[r.UserID]
		out[i] = StaffView{UserID: r.UserID, FirstName: n.first, LastInitial: lastInitial(n.last), Role: r.Role, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

type userName struct{ first, last string }

// resource builds the authz view of e for a (sensitive flag and the
// caller's staff role).
func (s *service) resource(ctx context.Context, a authz.Actor, e Event) (authz.Resource, error) {
	sens, err := s.sensitive(ctx, e)
	if err != nil {
		return authz.Resource{}, err
	}
	role, err := s.staffRole(ctx, a, e)
	if err != nil {
		return authz.Resource{}, err
	}
	return eventResource(e, sens, role), nil
}
