package events

import (
	"context"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/page"
)

// maxFollowedClubs bounds the followed clubs listMyClubEvents reads from
// feed (the newest follows first): the org filter of one events query.
const maxFollowedClubs = 500

// MyEventsQuery is a page of listMyStaffEvents / listMyClubEvents (BE-095).
type MyEventsQuery struct {
	Limit int
	After *After
}

// StaffEventView is an event of the caller's door list (BE-095): the card,
// the caller's staff role on it ("" when it works the door as one of the
// organiser's managers or as platform_admin of the platform org) and
// whether it may also void scans and close the attendance
// (events.checkin.void, events.attendance.finalize: Em, S, k-po).
type StaffEventView struct {
	EventView
	StaffRole StaffRole
	Manage    bool
}

// StaffEventPage is a page of the caller's door list. Next is the keyset
// position of the next page (nil: last page).
type StaffEventPage struct {
	Items []StaffEventView
	Next  *After
}

// myEvents is the resource of the caller's own event lists (cells O).
func myEvents(a authz.Actor) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: authz.TypeMyEvents, OwnerUserID: &me}
}

// doorOrgs are the organisers whose events a's org roles let it work the
// door of (03 §5.6 events.checkin.create, cell S of club_admin and
// university_rep, career_staff included; k-po: platform_admin for the
// platform org). The authz check of each listed event decides.
func doorOrgs(a authz.Actor) []uuid.UUID {
	out := []uuid.UUID{}
	pad := a.HasPlatformRole(authz.PlatformAdmin)
	for _, m := range a.Memberships {
		manager := m.Role == authz.MemberClubAdmin || m.Role == authz.MemberUniversityRep || m.Role == authz.MemberCareerStaff
		if (manager || (pad && m.OrgKind == authz.OrgPlatform)) && !slices.Contains(out, m.OrgID) {
			out = append(out, m.OrgID)
		}
	}
	return out
}

// ListMyStaffEvents implements Service (events.staff_event.list_own, O).
// The store lists the published events that have not ended, staffed by
// the caller or organised by an org it manages events of; each one is
// then authorised as the door would be (events.checkin.create with the
// caller's staff role and the sensitive flag), so the list never shows an
// event the caller cannot scan for. A refused event leaves its page one
// item short (the next page goes on from the store's position).
func (s *service) ListMyStaffEvents(ctx context.Context, a authz.Actor, q MyEventsQuery) (StaffEventPage, error) {
	if err := authz.Can(ctx, a, authz.EventsStaffEventListOwn, myEvents(a)); err != nil {
		return StaffEventPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	f := StaffEventFilter{UserID: a.UserID, OrgIDs: doorOrgs(a), EndsAfter: s.now()}
	rows, err := s.d.Store.ListStaffEvents(ctx, f, q.After, limit+1)
	if err != nil {
		return StaffEventPage{}, err
	}
	out := StaffEventPage{Items: []StaffEventView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.StartsAt, ID: last.ID}
	}
	if len(rows) == 0 {
		return out, nil
	}
	ids, orgIDs := make([]uuid.UUID, len(rows)), []uuid.UUID{}
	for i, e := range rows {
		ids[i] = e.ID
		if !slices.Contains(orgIDs, e.OrgID) {
			orgIDs = append(orgIDs, e.OrgID)
		}
	}
	roles, err := s.d.Store.StaffRolesOf(ctx, a.UserID, ids)
	if err != nil {
		return StaffEventPage{}, err
	}
	sens, err := s.d.Catalog.SensitiveOrgIDs(ctx, orgIDs)
	if err != nil {
		return StaffEventPage{}, err
	}
	os, err := s.d.Orgs.GetMany(ctx, orgIDs)
	if err != nil {
		return StaffEventPage{}, err
	}
	for _, e := range rows {
		o, ok := os[e.OrgID]
		if !ok {
			continue // an org row never disappears; a missing one is not shown
		}
		res := eventResource(e, e.IsSensitiveCategory || sens[e.OrgID], roles[e.ID])
		if authz.Can(ctx, a, authz.EventsCheckinCreate, res) != nil {
			continue
		}
		out.Items = append(out.Items, StaffEventView{
			EventView: s.view(ctx, e, o, false), StaffRole: roles[e.ID],
			Manage: authz.Can(ctx, a, authz.EventsCheckinVoid, res) == nil,
		})
	}
	return out, nil
}

// ListMyClubEvents implements Service (events.club_event.list_own, O). The
// clubs are the caller's active club memberships (the actor's, from orgs)
// and the clubs it follows (feed, through Follows); the events are read
// with listEvents' store filter and the caller's viewer (03 §3.6): only
// published events, a members event only of a club the caller is a member
// of or staff of the event, a university event only for its university's
// verified students, the club's members or the staff. A followed
// sensitive club therefore shows only its public events, as listEvents
// does. Events that have ended are left out.
func (s *service) ListMyClubEvents(ctx context.Context, a authz.Actor, q MyEventsQuery) (EventPage, error) {
	if err := authz.Can(ctx, a, authz.EventsClubEventListOwn, myEvents(a)); err != nil {
		return EventPage{}, err
	}
	clubs := []uuid.UUID{}
	for _, m := range a.Memberships {
		if m.OrgKind == authz.OrgClub && !slices.Contains(clubs, m.OrgID) {
			clubs = append(clubs, m.OrgID)
		}
	}
	if s.d.Follows != nil {
		followed, err := s.d.Follows.FollowedClubIDs(ctx, a, maxFollowedClubs)
		if err != nil {
			return EventPage{}, err
		}
		for _, id := range followed {
			if !slices.Contains(clubs, id) {
				clubs = append(clubs, id)
			}
		}
	}
	empty := EventPage{Items: []EventView{}, Sort: SortStartsAt}
	if len(clubs) == 0 {
		return empty, nil
	}
	now := s.now()
	f := ListFilter{Viewer: viewerOf(a), OrgIDs: clubs, EndsAfter: &now, Sort: SortStartsAt}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListEvents(ctx, f, q.After, limit+1)
	if err != nil {
		return EventPage{}, err
	}
	return s.page(ctx, rows, limit, SortStartsAt, func(Event) bool { return false })
}
