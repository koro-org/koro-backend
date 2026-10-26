package events

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the 12 event core operations of BE-024, the 7 form and
// lint rule operations of BE-026, the 8 registration operations of BE-028,
// the 9 ticket, check-in and attendance operations of BE-031, the 3
// export operations of BE-050 and the 2 own event lists of BE-095 (04
// §2.6) to api. svc may be nil (`koro
// openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "events", huma.Operation{
		OperationID: "listEvents", Method: http.MethodGet, Path: "/events",
		Summary: "List events",
		Description: "Published events the caller may see (anonymous: public events only). Without a date range, events " +
			"that have ended are left out. q matches title, summary and description (full text) or part of the title. " +
			"Repeated org_id / university_id values are ORed; the two filters together intersect. A date-only " +
			"starts_from / starts_to is the start / end of that day in Europe/Istanbul.",
	}, op.Meta{Action: string(authz.EventsEventList), Surface: op.SurfacePublic}, h.listEvents)

	op.Register(api, "events", huma.Operation{
		OperationID: "getEvent", Method: http.MethodGet, Path: "/events/{event_id}",
		Summary: "Get an event",
		Description: "An event the caller may not see is 404. online_url is shown to the event's managers only. The " +
			"ETag is the If-Match value of updateEvent.",
	}, op.Meta{Action: string(authz.EventsEventRead), Surface: op.SurfacePublic}, h.getEvent)

	op.Register(api, "events", huma.Operation{
		OperationID: "listOrgEvents", Method: http.MethodGet, Path: "/orgs/{org_id}/events",
		Summary: "List an organization's events", Description: "Drafts included; sorted by starts_at.",
	}, op.Meta{Action: string(authz.EventsEventListOrg), Surface: op.SurfaceOrg}, h.listOrgEvents)

	op.Register(api, "events", huma.Operation{
		OperationID: "createEvent", Method: http.MethodPost, Path: "/orgs/{org_id}/events", DefaultStatus: http.StatusCreated,
		Summary: "Create an event (draft)",
		Description: "Clubs, universities and the platform org organise events (companies: 403 in v1). The event starts " +
			"as a draft with an empty registration form draft. A suspended org cannot create (403 orgs.org_suspended).",
	}, op.Meta{Action: string(authz.EventsEventCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.createEvent)

	op.Register(api, "events", huma.Operation{
		OperationID: "updateEvent", Method: http.MethodPatch, Path: "/events/{event_id}",
		Summary: "Update an event",
		Description: "If-Match with the ETag of getEvent is required: missing → 428, stale → 412. Cancelled and " +
			"completed events cannot change (409 events.invalid_state). A schedule or venue change of a published " +
			"event notifies its attendees. clear lists the optional fields to empty.",
	}, op.Meta{Action: string(authz.EventsEventUpdate), Surface: op.SurfaceResource}, h.updateEvent)

	op.Register(api, "events", huma.Operation{
		OperationID: "publishEvent", Method: http.MethodPost, Path: "/events/{event_id}/publish",
		Summary: "Publish a draft event",
		Description: "draft → published. An event with registration needs a published form version (422 " +
			"events.form_required); an online or hybrid event needs online_url; a past event cannot be published.",
	}, op.Meta{Action: string(authz.EventsEventPublish), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.publishEvent)

	op.Register(api, "events", huma.Operation{
		OperationID: "cancelEvent", Method: http.MethodPost, Path: "/events/{event_id}/cancel",
		Summary: "Cancel a published event",
		Description: "published → cancelled. Registrations and tickets are cancelled and attendees notified " +
			"(event.cancelled).",
	}, op.Meta{Action: string(authz.EventsEventCancel), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.cancelEvent)

	op.Register(api, "events", huma.Operation{
		OperationID: "deleteEventDraft", Method: http.MethodDelete, Path: "/events/{event_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Delete a draft event", Description: "Drafts only (409 events.invalid_state otherwise).",
	}, op.Meta{Action: string(authz.EventsEventDelete), Surface: op.SurfaceResource}, h.deleteEvent)

	op.Register(api, "events", huma.Operation{
		OperationID: "listEventStaff", Method: http.MethodGet, Path: "/events/{event_id}/staff",
		Summary: "List an event's staff", Description: "First name and the initial of the last name only.",
	}, op.Meta{Action: string(authz.EventsStaffList), Surface: op.SurfaceResource}, h.listStaff)

	op.Register(api, "events", huma.Operation{
		OperationID: "addEventStaff", Method: http.MethodPost, Path: "/events/{event_id}/staff", DefaultStatus: http.StatusCreated,
		Summary: "Add a staff member",
		Description: "The address must belong to an active account (404 otherwise; invitations come later). Staff are " +
			"managed while the event is a draft or published.",
	}, op.Meta{Action: string(authz.EventsStaffManage), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.addStaff)

	op.Register(api, "events", huma.Operation{
		OperationID: "updateEventStaff", Method: http.MethodPatch, Path: "/events/{event_id}/staff/{user_id}",
		Summary: "Change a staff member's role",
	}, op.Meta{Action: string(authz.EventsStaffManage), Surface: op.SurfaceResource}, h.updateStaff)

	op.Register(api, "events", huma.Operation{
		OperationID: "removeEventStaff", Method: http.MethodDelete, Path: "/events/{event_id}/staff/{user_id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Remove a staff member",
	}, op.Meta{Action: string(authz.EventsStaffManage), Surface: op.SurfaceResource}, h.removeStaff)

	registerForms(api, h)
	registerRegistrations(api, h)
	registerCheckins(api, h)
	registerExports(api, h)
	registerMine(api, h)
}

type handler struct{ svc Service }

// EventPath is the event_id path parameter (exported: huma reads the
// fields of an embedded struct only when its type is exported).
type EventPath struct {
	EventID string `path:"event_id" doc:"Event ID"`
}

func (p EventPath) id() uuid.UUID { return httpx.ParseID(p.EventID) }

// OrgPath is the org_id path parameter.
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID"`
}

func (p OrgPath) id() uuid.UUID { return httpx.ParseID(p.OrgID) }

type eventOutput struct {
	ETag string `header:"ETag"`
	Body EventDTO
}

func eventOut(v EventView) *eventOutput { return &eventOutput{ETag: v.ETag, Body: toEventDTO(v)} }

type listOutput struct {
	Body EventCardList
}

// ---- listEvents

type listEventsInput struct {
	page.Params
	Q            string   `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	OrgID        []string `query:"org_id,explode" maxItems:"20" doc:"Organiser org IDs (repeat the parameter)"`
	UniversityID []string `query:"university_id,explode" maxItems:"20" doc:"University IDs: the university's own org and its clubs"`
	Category     string   `query:"category" enum:"workshop,talk,competition,career,social,trip,sports,culture,volunteering,other"`
	Format       string   `query:"format" enum:"in_person,online,hybrid"`
	City         string   `query:"city" maxLength:"60"`
	StartsFrom   string   `query:"starts_from" maxLength:"40" doc:"RFC 3339 instant or YYYY-MM-DD (Europe/Istanbul day start); inclusive"`
	StartsTo     string   `query:"starts_to" maxLength:"40" doc:"RFC 3339 instant or YYYY-MM-DD (Europe/Istanbul day end); exclusive"`
	Sort         string   `query:"sort" enum:"starts_at,-published_at" doc:"starts_at (default, soonest first) or -published_at"`
}

func (h handler) listEvents(ctx context.Context, in *listEventsInput) (*listOutput, error) {
	q := ListQuery{Q: in.Q, Category: Category(in.Category), Format: Format(in.Format), City: in.City, Sort: in.Sort, Limit: in.Limit}
	if q.Sort == "" {
		q.Sort = SortStartsAt
	}
	var err error
	if q.OrgIDs, err = parseIDs("query.org_id", in.OrgID); err != nil {
		return nil, err
	}
	if q.UniversityIDs, err = parseIDs("query.university_id", in.UniversityID); err != nil {
		return nil, err
	}
	if q.StartsFrom, err = parseBound("query.starts_from", in.StartsFrom, false); err != nil {
		return nil, err
	}
	if q.StartsTo, err = parseBound("query.starts_to", in.StartsTo, true); err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listEvents", in.Q, in.Category, in.Format, in.City, in.StartsFrom, in.StartsTo,
		strings.Join(in.OrgID, ","), strings.Join(in.UniversityID, ","))
	if q.After, err = cursorOf(in.Cursor, q.Sort, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListEvents(ctx, authz.ActorOrAnonymous(ctx), q)
	if err != nil {
		return nil, err
	}
	return cardsOut(p, fp), nil
}

func parseIDs(loc string, raw []string) ([]uuid.UUID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, problem.Field(loc, "must be a UUID")
		}
		out = append(out, id)
	}
	return out, nil
}

// istanbul is the zone of date-only filters (04 §1.7, §1.12); the binary
// embeds time/tzdata, the fixed offset is a fallback only.
var istanbul = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Istanbul"); err == nil {
		return loc
	}
	return time.FixedZone("TRT", 3*60*60)
})

// parseBound parses a range bound: an RFC 3339 instant, or a date that is
// the start of that Istanbul day (end: the start of the next day, so the
// exclusive bound covers the whole day).
func parseBound(loc, s string, end bool) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		t = t.UTC()
		return &t, nil
	}
	d, err := time.ParseInLocation(time.DateOnly, s, istanbul())
	if err != nil {
		return nil, problem.Field(loc, "must be an RFC 3339 time or a YYYY-MM-DD date")
	}
	if end {
		d = d.AddDate(0, 0, 1)
	}
	d = d.UTC()
	return &d, nil
}

// cursorOf decodes a cursor of sort with the request's filter fingerprint.
func cursorOf(raw, sort, fp string) (*After, error) {
	c, err := page.Decode(raw, sort, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &After{At: at, ID: id}, nil
}

func cardsOut(p EventPage, fp string) *listOutput {
	out := &listOutput{Body: EventCardList{Items: make([]EventCardDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toCardDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(p.Sort, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out
}

// ---- getEvent

func (h handler) getEvent(ctx context.Context, in *EventPath) (*eventOutput, error) {
	v, err := h.svc.GetEvent(ctx, authz.ActorOrAnonymous(ctx), in.id())
	if err != nil {
		return nil, err
	}
	return eventOut(v), nil
}

// ---- listOrgEvents

type listOrgEventsInput struct {
	OrgPath
	page.Params
	Status []string `query:"status,explode" maxItems:"4" enum:"draft,published,cancelled,completed" doc:"Statuses (repeat the parameter)"`
	Sort   string   `query:"sort" enum:"starts_at" doc:"starts_at (the only order)"`
}

func (h handler) listOrgEvents(ctx context.Context, in *listOrgEventsInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q := OrgListQuery{Limit: in.Limit}
	for _, s := range in.Status {
		q.Statuses = append(q.Statuses, Status(s))
	}
	fp := page.Fingerprint("listOrgEvents", in.OrgID, strings.Join(in.Status, ","))
	if q.After, err = cursorOf(in.Cursor, SortStartsAt, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListOrgEvents(ctx, a, in.id(), q)
	if err != nil {
		return nil, err
	}
	return cardsOut(p, fp), nil
}

// ---- createEvent

// EventWriteBody is EventWrite of 04 §2.6 (createEvent).
type EventWriteBody struct {
	Title                  string         `json:"title" minLength:"3" maxLength:"160"`
	Slug                   string         `json:"slug,omitempty" maxLength:"100" doc:"Derived from the title when omitted"`
	Summary                string         `json:"summary,omitempty" maxLength:"300"`
	Description            map[string]any `json:"description_json,omitempty" doc:"Tiptap JSON: paragraphs, headings 2-3, lists, quotes, bold, italic, https links"`
	Category               string         `json:"category" enum:"workshop,talk,competition,career,social,trip,sports,culture,volunteering,other"`
	Format                 string         `json:"format" enum:"in_person,online,hybrid"`
	LocationName           string         `json:"location_name,omitempty" maxLength:"200"`
	Address                string         `json:"address,omitempty" maxLength:"500"`
	City                   string         `json:"city,omitempty" maxLength:"60"`
	OnlineURL              string         `json:"online_url,omitempty" maxLength:"2048" doc:"https:// only; required to publish an online or hybrid event"`
	StartsAt               time.Time      `json:"starts_at"`
	EndsAt                 time.Time      `json:"ends_at"`
	CoverFileID            string         `json:"cover_file_id,omitempty" doc:"An event_cover upload (createUpload)"`
	Visibility             string         `json:"visibility,omitempty" enum:"public,university,members" doc:"Default public"`
	VisibilityUniversityID string         `json:"visibility_university_id,omitempty" doc:"Required for university visibility"`
	RequiresRegistration   *bool          `json:"requires_registration,omitempty" doc:"Default true"`
	RegistrationMode       string         `json:"registration_mode,omitempty" enum:"auto,manual" doc:"Default auto"`
	Capacity               *int           `json:"capacity,omitempty" minimum:"1" maximum:"100000"`
	WaitlistEnabled        bool           `json:"waitlist_enabled,omitempty"`
	RegistrationOpensAt    *time.Time     `json:"registration_opens_at,omitempty"`
	RegistrationClosesAt   *time.Time     `json:"registration_closes_at,omitempty"`
	CancelDeadlineAt       *time.Time     `json:"cancel_deadline_at,omitempty"`
	CheckinEnabled         bool           `json:"checkin_enabled,omitempty"`
	IsSensitiveCategory    bool           `json:"is_sensitive_category,omitempty" doc:"Religious, political etc.: produces no talent signal"`
}

type createEventInput struct {
	OrgPath
	Body EventWriteBody
}

func (h handler) createEvent(ctx context.Context, in *createEventInput) (*eventOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	ev := EventInput{
		Title: b.Title, Slug: b.Slug, Summary: b.Summary, Description: b.Description, Category: Category(b.Category),
		Format: Format(b.Format), LocationName: b.LocationName, Address: b.Address, City: b.City, OnlineURL: b.OnlineURL,
		StartsAt: b.StartsAt, EndsAt: b.EndsAt, Visibility: Visibility(b.Visibility),
		RequiresRegistration: b.RequiresRegistration, RegistrationMode: RegistrationMode(b.RegistrationMode),
		Capacity: b.Capacity, WaitlistEnabled: b.WaitlistEnabled, RegistrationOpensAt: b.RegistrationOpensAt,
		RegistrationClosesAt: b.RegistrationClosesAt, CancelDeadlineAt: b.CancelDeadlineAt, CheckinEnabled: b.CheckinEnabled,
		IsSensitiveCategory: b.IsSensitiveCategory,
	}
	if ev.CoverFileID, err = httpx.OptID("body.cover_file_id", b.CoverFileID); err != nil {
		return nil, err
	}
	if ev.VisibilityUniversityID, err = httpx.OptID("body.visibility_university_id", b.VisibilityUniversityID); err != nil {
		return nil, err
	}
	v, err := h.svc.CreateEvent(ctx, a, in.id(), ev)
	if err != nil {
		return nil, err
	}
	return eventOut(v), nil
}

// ---- updateEvent

// EventPatchBody is "EventWrite kısmi" of 04 §2.6: an absent field stays as
// it is; clear empties optional fields.
type EventPatchBody struct {
	Title                  *string        `json:"title,omitempty" minLength:"3" maxLength:"160"`
	Slug                   *string        `json:"slug,omitempty" maxLength:"100"`
	Summary                *string        `json:"summary,omitempty" maxLength:"300" doc:"Empty string clears it"`
	Description            map[string]any `json:"description_json,omitempty" doc:"Tiptap JSON"`
	Category               *string        `json:"category,omitempty" enum:"workshop,talk,competition,career,social,trip,sports,culture,volunteering,other"`
	Format                 *string        `json:"format,omitempty" enum:"in_person,online,hybrid"`
	LocationName           *string        `json:"location_name,omitempty" maxLength:"200"`
	Address                *string        `json:"address,omitempty" maxLength:"500"`
	City                   *string        `json:"city,omitempty" maxLength:"60"`
	OnlineURL              *string        `json:"online_url,omitempty" maxLength:"2048"`
	StartsAt               *time.Time     `json:"starts_at,omitempty"`
	EndsAt                 *time.Time     `json:"ends_at,omitempty"`
	CoverFileID            *string        `json:"cover_file_id,omitempty"`
	Visibility             *string        `json:"visibility,omitempty" enum:"public,university,members"`
	VisibilityUniversityID *string        `json:"visibility_university_id,omitempty"`
	RequiresRegistration   *bool          `json:"requires_registration,omitempty"`
	RegistrationMode       *string        `json:"registration_mode,omitempty" enum:"auto,manual"`
	Capacity               *int           `json:"capacity,omitempty" minimum:"1" maximum:"100000"`
	WaitlistEnabled        *bool          `json:"waitlist_enabled,omitempty"`
	RegistrationOpensAt    *time.Time     `json:"registration_opens_at,omitempty"`
	RegistrationClosesAt   *time.Time     `json:"registration_closes_at,omitempty"`
	CancelDeadlineAt       *time.Time     `json:"cancel_deadline_at,omitempty"`
	CheckinEnabled         *bool          `json:"checkin_enabled,omitempty"`
	IsSensitiveCategory    *bool          `json:"is_sensitive_category,omitempty"`
	Clear                  []string       `json:"clear,omitempty" maxItems:"11" doc:"Optional fields to empty" enum:"summary,description_json,location_name,address,city,online_url,cover_file_id,capacity,registration_opens_at,registration_closes_at,cancel_deadline_at"`
}

type updateEventInput struct {
	EventPath
	IfMatch string `header:"If-Match" doc:"ETag of getEvent (required: missing → 428, stale → 412)"`
	Body    EventPatchBody
}

func (h handler) updateEvent(ctx context.Context, in *updateEventInput) (*eventOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := EventPatch{
		Title: b.Title, Slug: b.Slug, Summary: b.Summary, Description: b.Description, LocationName: b.LocationName,
		Address: b.Address, City: b.City, OnlineURL: b.OnlineURL, StartsAt: b.StartsAt, EndsAt: b.EndsAt,
		RequiresRegistration: b.RequiresRegistration, Capacity: b.Capacity, WaitlistEnabled: b.WaitlistEnabled,
		RegistrationOpensAt: b.RegistrationOpensAt, RegistrationClosesAt: b.RegistrationClosesAt,
		CancelDeadlineAt: b.CancelDeadlineAt, CheckinEnabled: b.CheckinEnabled, IsSensitiveCategory: b.IsSensitiveCategory,
		Clear: b.Clear,
	}
	if b.Category != nil {
		p.Category = ptr(Category(*b.Category))
	}
	if b.Format != nil {
		p.Format = ptr(Format(*b.Format))
	}
	if b.Visibility != nil {
		p.Visibility = ptr(Visibility(*b.Visibility))
	}
	if b.RegistrationMode != nil {
		p.RegistrationMode = ptr(RegistrationMode(*b.RegistrationMode))
	}
	if b.CoverFileID != nil {
		if p.CoverFileID, err = httpx.OptID("body.cover_file_id", *b.CoverFileID); err != nil || p.CoverFileID == nil {
			return nil, firstErr(err, problem.Field("body.cover_file_id", "must be a UUID (use clear to remove the cover)"))
		}
	}
	if b.VisibilityUniversityID != nil {
		if p.VisibilityUniversityID, err = httpx.OptID("body.visibility_university_id", *b.VisibilityUniversityID); err != nil ||
			p.VisibilityUniversityID == nil {
			return nil, firstErr(err, problem.Field("body.visibility_university_id", "must be a UUID"))
		}
	}
	v, err := h.svc.UpdateEvent(ctx, a, in.id(), in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return eventOut(v), nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// ---- publishEvent / cancelEvent / deleteEventDraft

func (h handler) publishEvent(ctx context.Context, in *EventPath) (*eventOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.PublishEvent(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return eventOut(v), nil
}

type cancelEventInput struct {
	EventPath
	Body struct {
		CancelReason eventCancelReason `json:"cancel_reason" doc:"Shown to the attendees"`
	}
}

func (h handler) cancelEvent(ctx context.Context, in *cancelEventInput) (*eventOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CancelEvent(ctx, a, in.id(), string(in.Body.CancelReason))
	if err != nil {
		return nil, err
	}
	return eventOut(v), nil
}

func (h handler) deleteEvent(ctx context.Context, in *EventPath) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.DeleteEventDraft(ctx, a, in.id())
}

// ---- staff

type staffListOutput struct {
	Body StaffList
}

func (h handler) listStaff(ctx context.Context, in *EventPath) (*staffListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.ListStaff(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	out := &staffListOutput{Body: StaffList{Items: make([]StaffDTO, len(vs))}}
	for i, v := range vs {
		out.Body.Items[i] = toStaffDTO(v)
	}
	return out, nil
}

type staffOutput struct {
	Body StaffDTO
}

type addStaffInput struct {
	EventPath
	Body struct {
		Email string `json:"email" minLength:"3" maxLength:"254" doc:"Address of an existing account"`
		Role  string `json:"role" enum:"scanner,manager"`
	}
}

func (h handler) addStaff(ctx context.Context, in *addStaffInput) (*staffOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AddStaff(ctx, a, in.id(), in.Body.Email, StaffRole(in.Body.Role))
	if err != nil {
		return nil, err
	}
	return &staffOutput{Body: toStaffDTO(v)}, nil
}

// StaffPath is the event_id and user_id path parameters.
type StaffPath struct {
	EventPath
	UserID string `path:"user_id" doc:"Staff member's user ID"`
}

type updateStaffInput struct {
	StaffPath
	Body struct {
		Role string `json:"role" enum:"scanner,manager"`
	}
}

func (h handler) updateStaff(ctx context.Context, in *updateStaffInput) (*staffOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.UpdateStaff(ctx, a, in.id(), httpx.ParseID(in.UserID), StaffRole(in.Body.Role))
	if err != nil {
		return nil, err
	}
	return &staffOutput{Body: toStaffDTO(v)}, nil
}

func (h handler) removeStaff(ctx context.Context, in *StaffPath) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.RemoveStaff(ctx, a, in.id(), httpx.ParseID(in.UserID))
}
