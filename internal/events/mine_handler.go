package events

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
)

// registerMine adds the 2 operations of BE-095 (04 §2.6; koro-mobile B-04,
// B-05): the caller's door list and the events of its clubs.
func registerMine(api huma.API, h handler) {
	reg(api, huma.Operation{
		OperationID: "listMyStaffEvents", Method: http.MethodGet, Path: "/me/staff-events",
		Summary: "List the events whose door I work",
		Description: "Upcoming and running published events (not ended yet) the caller may scan tickets for: the " +
			"events it is staff of (scanner or manager) and the events of the orgs it manages events of (club_admin, " +
			"university_rep, career_staff; platform_admin for the platform org). Every listed event passes the same " +
			"authorization as createCheckin. door_access tells what the caller may do at the door; checkin_enabled " +
			"tells whether scanning is on. Sorted by starts_at (soonest first); no total count.",
	}, op.Meta{Action: string(authz.EventsStaffEventListOwn), Surface: op.SurfaceSelf}, h.listMyStaffEvents)

	reg(api, huma.Operation{
		OperationID: "listMyClubEvents", Method: http.MethodGet, Path: "/me/club-events",
		Summary: "List upcoming events of my clubs",
		Description: "Upcoming and running published events (not ended yet) of the clubs the caller is an active " +
			"member of or follows, in one list. Visibility is listEvents' (a members-only event only for the club's " +
			"members and the event's staff, a university event only for that university's verified students, the " +
			"club's members and the staff), so a followed club's members-only events are not listed. The response " +
			"does not say whether a club is followed or joined. Sorted by starts_at (soonest first); no total count.",
	}, op.Meta{Action: string(authz.EventsClubEventListOwn), Surface: op.SurfaceSelf}, h.listMyClubEvents)
}

// StaffEventDTO is an event of the caller's door list (listMyStaffEvents):
// the event card plus what the caller may do at its door.
type StaffEventDTO struct {
	EventCardDTO
	CheckinEnabled bool   `json:"checkin_enabled" doc:"Whether check-in is on for the event (createCheckin answers 409 otherwise)"`
	StaffRole      string `json:"staff_role,omitempty" enum:"scanner,manager" doc:"The caller's staff role on the event; absent when it works the door as one of the organiser's managers"`
	DoorAccess     string `json:"door_access" enum:"scanner,manager" doc:"scanner: scan tickets, list check-ins and read the counters; manager: also void check-ins and close the attendance"`
}

// StaffEventList is a page of the caller's door list.
type StaffEventList struct {
	Items      []StaffEventDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty" doc:"Cursor of the next page; absent on the last page"`
}

type myEventsInput struct {
	page.Params
}

type staffEventsOutput struct {
	Body StaffEventList
}

func (h handler) listMyStaffEvents(ctx context.Context, in *myEventsInput) (*staffEventsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyStaffEvents")
	q := MyEventsQuery{Limit: in.Limit}
	if q.After, err = cursorOf(in.Cursor, SortStartsAt, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListMyStaffEvents(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &staffEventsOutput{Body: StaffEventList{Items: make([]StaffEventDTO, len(p.Items))}}
	for i, v := range p.Items {
		access := "scanner"
		if v.Manage {
			access = "manager"
		}
		out.Body.Items[i] = StaffEventDTO{EventCardDTO: toCardDTO(v.EventView), CheckinEnabled: v.Event.CheckinEnabled,
			StaffRole: string(v.StaffRole), DoorAccess: access}
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(SortStartsAt, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

func (h handler) listMyClubEvents(ctx context.Context, in *myEventsInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyClubEvents")
	q := MyEventsQuery{Limit: in.Limit}
	if q.After, err = cursorOf(in.Cursor, SortStartsAt, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListMyClubEvents(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return cardsOut(p, fp), nil
}
