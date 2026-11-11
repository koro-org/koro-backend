package ingest

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the ingest operations (04 §2.25 doc patch; BE-210) to api.
// svc may be nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "ingest", huma.Operation{
		OperationID: "listImportedEvents", Method: http.MethodGet, Path: "/imported-events",
		Summary: "List the upcoming events collected from official pages",
		Description: "Events read automatically from the universities' and the clubs' own public pages (kind imported): " +
			"show each with label \"Otomatik toplandı\", source_host and a link to source_url. Not Koro events: no " +
			"registration, no talent point, no recommendation. Upcoming only (not ended), by start.",
	}, op.Meta{Action: string(authz.IngestImportedEventList), Surface: op.SurfacePublic}, h.listEvents)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "getImportedEvent", Method: http.MethodGet, Path: "/imported-events/{imported_event_id}",
		Summary: "Get an event collected from an official page",
	}, op.Meta{Action: string(authz.IngestImportedEventRead), Surface: op.SurfacePublic}, h.getEvent)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "requestImportedEventRemoval", Method: http.MethodPost,
		Path: "/imported-events/{imported_event_id}/removal-requests", DefaultStatus: http.StatusAccepted,
		Summary: "Ask that a collected event be removed",
		Description: "No account needed. The event is hidden at once and a platform admin reviews it. The body takes a " +
			"reason only (no name, no address).",
	}, op.Meta{Action: string(authz.IngestImportedEventRequestRemoval), Surface: op.SurfacePublic,
		Idempotency: op.IdempotencyOptional}, h.requestRemoval)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "listClubImportedEvents", Method: http.MethodGet, Path: "/orgs/{org_id}/imported-events",
		Summary: "List the club's collected events (suggestions)",
		Description: "The club's upcoming events read from official pages, with the quote they were read from. Promote " +
			"one to make it a draft event, or reject it.",
	}, op.Meta{Action: string(authz.IngestSuggestionList), Surface: op.SurfaceOrg}, h.listSuggestions)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "promoteImportedEvent", Method: http.MethodPost,
		Path: "/orgs/{org_id}/imported-events/{imported_event_id}/promote", DefaultStatus: http.StatusCreated,
		Summary: "Make a collected event a draft event of the club",
		Description: "Creates an ordinary draft (title, time, place, category; events.event.create applies) and marks " +
			"the collected event promoted. Complete and publish the draft with the event operations.",
	}, op.Meta{Action: string(authz.IngestSuggestionPromote), Surface: op.SurfaceOrg,
		Idempotency: op.IdempotencyOptional}, h.promote)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "rejectImportedEvent", Method: http.MethodPost,
		Path: "/orgs/{org_id}/imported-events/{imported_event_id}/reject", DefaultStatus: http.StatusNoContent,
		Summary: "Decline a collected event of the club",
	}, op.Meta{Action: string(authz.IngestSuggestionReject), Surface: op.SurfaceOrg,
		Idempotency: op.IdempotencyOptional}, h.reject)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminListIngestSources", Method: http.MethodGet, Path: "/admin/ingest/sources",
		Summary: "List the collection sources",
	}, op.Meta{Action: string(authz.IngestSourceList), Surface: op.SurfaceAdmin}, h.adminListSources)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminCreateIngestSource", Method: http.MethodPost, Path: "/admin/ingest/sources",
		DefaultStatus: http.StatusCreated,
		Summary:       "Add a collection source",
		Description: "A public http(s) page (ports 80/443, no credentials, no private or local address). The adapter must " +
			"yield the kind. Starts enabled or disabled per the environment unless enabled is given.",
	}, op.Meta{Action: string(authz.IngestSourceCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.adminCreateSource)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminAddPilotIngestSources", Method: http.MethodPost, Path: "/admin/ingest/pilot-sources",
		Summary: "Add the built-in sources of a pilot university",
		Description: "The official club lists and event pages of YTÜ, Boğaziçi, Sabancı, ODTÜ and İTÜ for the given " +
			"university of the catalog. A URL that exists is kept as it is.",
	}, op.Meta{Action: string(authz.IngestSourceCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.adminAddPilot)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminGetIngestSource", Method: http.MethodGet, Path: "/admin/ingest/sources/{source_id}",
		Summary: "Get a collection source with its latest runs",
	}, op.Meta{Action: string(authz.IngestSourceRead), Surface: op.SurfaceAdmin}, h.adminGetSource)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminUpdateIngestSource", Method: http.MethodPost, Path: "/admin/ingest/sources/{source_id}/settings",
		Summary: "Enable, disable or reschedule a collection source",
		Description: "Enabling clears the pause (failures, robots, claimed) and makes the source due now; disabling " +
			"records paused_reason admin.",
	}, op.Meta{Action: string(authz.IngestSourceUpdate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.adminUpdateSource)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminRunIngestSource", Method: http.MethodPost, Path: "/admin/ingest/sources/{source_id}/run",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Run an enabled collection source now",
		Description:   "The next collection tick (every minute) fetches it, under robots.txt and the host gap.",
	}, op.Meta{Action: string(authz.IngestSourceRun), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.adminRunSource)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminListImportedEvents", Method: http.MethodGet, Path: "/admin/ingest/imported-events",
		Summary: "List the collected events (every status)",
	}, op.Meta{Action: string(authz.IngestImportedEventListAdmin), Surface: op.SurfaceAdmin}, h.adminListEvents)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminSetImportedEventStatus", Method: http.MethodPost,
		Path:    "/admin/ingest/imported-events/{imported_event_id}/status",
		Summary: "Hide or restore a collected event",
	}, op.Meta{Action: string(authz.IngestImportedEventModerate), Surface: op.SurfaceAdmin,
		Idempotency: op.IdempotencyOptional}, h.adminSetStatus)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminDeleteImportedEvent", Method: http.MethodDelete,
		Path: "/admin/ingest/imported-events/{imported_event_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Delete a collected event",
	}, op.Meta{Action: string(authz.IngestImportedEventDelete), Surface: op.SurfaceAdmin}, h.adminDeleteEvent)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminListImportedClubs", Method: http.MethodGet, Path: "/admin/ingest/clubs",
		Summary: "List the clubs seen on official lists",
		Description: "needs_review=true: the clubs that started as other (a special category is never set " +
			"automatically, or the list's label is unknown).",
	}, op.Meta{Action: string(authz.IngestImportedClubList), Surface: op.SurfaceAdmin}, h.adminListClubs)

	op.Register(api, "ingest", huma.Operation{
		OperationID: "adminMarkImportedClubReviewed", Method: http.MethodPost, Path: "/admin/ingest/clubs/{club_id}/reviewed",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Mark an imported club's category as reviewed",
	}, op.Meta{Action: string(authz.IngestImportedClubReview), Surface: op.SurfaceAdmin,
		Idempotency: op.IdempotencyOptional}, h.adminMarkReviewed)
}

type handler struct{ svc Service }

// ImportedEventPath is the imported_event_id path parameter.
type ImportedEventPath struct {
	ImportedEventID string `path:"imported_event_id"`
}

// OrgPath is the org_id path parameter (a club).
type OrgPath struct {
	OrgID string `path:"org_id"`
}

// SourcePath is the source_id path parameter.
type SourcePath struct {
	SourceID string `path:"source_id"`
}

// ClubPath is the club_id path parameter (the club's org ID).
type ClubPath struct {
	ClubID string `path:"club_id"`
}

type noContent struct{}

// Cursor sort keys.
const (
	sortStart = "starts_at_asc"
	sortID    = "id_asc"
)

func eventCursor(raw, fp string) (*EventAfter, error) {
	c, err := page.Decode(raw, sortStart, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	t, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &EventAfter{StartsAt: t, ID: id}, nil
}

func nextEventCursor(n *EventAfter, fp string) string {
	if n == nil {
		return ""
	}
	return page.Next(sortStart, fp, page.TimeVal(n.StartsAt), page.UUIDVal(n.ID))
}

func idCursor(raw, fp string) (*uuid.UUID, error) {
	c, err := page.Decode(raw, sortID, fp, 1)
	if err != nil || c == nil {
		return nil, err
	}
	id, err := c.UUID(0)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func nextIDCursor(n *uuid.UUID, fp string) string {
	if n == nil {
		return ""
	}
	return page.Next(sortID, fp, page.UUIDVal(*n))
}

// optTime parses an optional RFC 3339 time.
func optTime(loc, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, problem.Field(loc, "must be an RFC 3339 date-time")
	}
	return &t, nil
}

func optBool(s string) *bool {
	if s == "" {
		return nil
	}
	v := s == "true"
	return &v
}

// ---- public

type listEventsInput struct {
	page.Params
	UniversityID string `query:"university_id" format:"uuid"`
	ClubID       string `query:"club_id" format:"uuid" doc:"The club's org ID"`
	From         string `query:"from" format:"date-time" doc:"Starts at or after"`
	To           string `query:"to" format:"date-time" doc:"Starts before"`
}

type listEventsOutput struct{ Body ImportedEventList }

func (in *listEventsInput) query(fp string) (EventQuery, error) {
	uni, err := httpx.OptID("query.university_id", in.UniversityID)
	if err != nil {
		return EventQuery{}, err
	}
	club, err := httpx.OptID("query.club_id", in.ClubID)
	if err != nil {
		return EventQuery{}, err
	}
	from, err := optTime("query.from", in.From)
	if err != nil {
		return EventQuery{}, err
	}
	to, err := optTime("query.to", in.To)
	if err != nil {
		return EventQuery{}, err
	}
	after, err := eventCursor(in.Cursor, fp)
	if err != nil {
		return EventQuery{}, err
	}
	return EventQuery{UniversityID: uni, ClubOrgID: club, From: from, To: to, Limit: in.Limit, After: after}, nil
}

func (h handler) listEvents(ctx context.Context, in *listEventsInput) (*listEventsOutput, error) {
	fp := page.Fingerprint("listImportedEvents", in.UniversityID, in.ClubID, in.From, in.To)
	q, err := in.query(fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListImportedEvents(ctx, authz.ActorOrAnonymous(ctx), q)
	if err != nil {
		return nil, err
	}
	out := &listEventsOutput{Body: ImportedEventList{Items: make([]ImportedEventDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toEventDTO(v)
	}
	out.Body.NextCursor = nextEventCursor(p.Next, fp)
	return out, nil
}

type eventOutput struct{ Body ImportedEventDTO }

func (h handler) getEvent(ctx context.Context, in *ImportedEventPath) (*eventOutput, error) {
	v, err := h.svc.GetImportedEvent(ctx, authz.ActorOrAnonymous(ctx), httpx.ParseID(in.ImportedEventID))
	if err != nil {
		return nil, err
	}
	return &eventOutput{Body: toEventDTO(v)}, nil
}

type removalInput struct {
	ImportedEventPath
	Body struct {
		Reason string `json:"reason" enum:"personal_data,incorrect,copyright,cancelled,other"`
	}
}

func (h handler) requestRemoval(ctx context.Context, in *removalInput) (*noContent, error) {
	err := h.svc.RequestRemoval(ctx, authz.ActorOrAnonymous(ctx), httpx.ParseID(in.ImportedEventID), RemovalReason(in.Body.Reason))
	if err != nil {
		return nil, err
	}
	return &noContent{}, nil
}

// ---- club

type listSuggestionsInput struct {
	OrgPath
	page.Params
}

type listSuggestionsOutput struct{ Body ImportedEventSuggestionList }

func (h handler) listSuggestions(ctx context.Context, in *listSuggestionsInput) (*listSuggestionsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listClubImportedEvents", in.OrgID)
	after, err := eventCursor(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListSuggestions(ctx, a, httpx.ParseID(in.OrgID), EventQuery{Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &listSuggestionsOutput{Body: ImportedEventSuggestionList{Items: make([]ImportedEventSuggestionDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toSuggestionDTO(v)
	}
	out.Body.NextCursor = nextEventCursor(p.Next, fp)
	return out, nil
}

type suggestionInput struct {
	OrgPath
	ImportedEventPath
}

type promotedOutput struct{ Body ImportedEventPromotedDTO }

func (h handler) promote(ctx context.Context, in *suggestionInput) (*promotedOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id := httpx.ParseID(in.ImportedEventID)
	v, err := h.svc.PromoteSuggestion(ctx, a, httpx.ParseID(in.OrgID), id)
	if err != nil {
		return nil, err
	}
	return &promotedOutput{Body: ImportedEventPromotedDTO{ImportedEventID: id, EventID: v.Event.ID}}, nil
}

func (h handler) reject(ctx context.Context, in *suggestionInput) (*noContent, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.RejectSuggestion(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.ImportedEventID)); err != nil {
		return nil, err
	}
	return &noContent{}, nil
}

// ---- admin: sources

type adminListSourcesInput struct {
	page.Params
	UniversityID string `query:"university_id" format:"uuid"`
	ClubID       string `query:"club_id" format:"uuid"`
	Kind         string `query:"kind" enum:"club_list,events"`
	Enabled      string `query:"enabled" enum:"true,false"`
}

type sourceListOutput struct{ Body IngestSourceList }

func (h handler) adminListSources(ctx context.Context, in *adminListSourcesInput) (*sourceListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	uni, err := httpx.OptID("query.university_id", in.UniversityID)
	if err != nil {
		return nil, err
	}
	club, err := httpx.OptID("query.club_id", in.ClubID)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListIngestSources", in.UniversityID, in.ClubID, in.Kind, in.Enabled)
	after, err := idCursor(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListSources(ctx, a, SourceQuery{Filter: SourceFilter{
		UniversityID: uni, ClubOrgID: club, Kind: Kind(in.Kind), Enabled: optBool(in.Enabled),
	}, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &sourceListOutput{Body: IngestSourceList{Items: make([]IngestSourceDTO, len(p.Items))}}
	for i, s := range p.Items {
		out.Body.Items[i] = toSourceDTO(s)
	}
	out.Body.NextCursor = nextIDCursor(p.Next, fp)
	return out, nil
}

type sourceOutput struct{ Body IngestSourceDetailDTO }

func (h handler) adminGetSource(ctx context.Context, in *SourcePath) (*sourceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminGetSource(ctx, a, httpx.ParseID(in.SourceID))
	if err != nil {
		return nil, err
	}
	return &sourceOutput{Body: toSourceDetail(v)}, nil
}

type adminCreateSourceInput struct {
	Body struct {
		Kind          string `json:"kind" enum:"club_list,events"`
		Adapter       string `json:"adapter" enum:"htmlpage,jsonapi,rss,sitemap,instagram_graph,manual_paste,clublist_ytu,clublist_boun,clublist_sabanci,clublist_metu,clublist_itu"`
		URL           string `json:"url" maxLength:"2048"`
		Label         string `json:"label" minLength:"1" maxLength:"120"`
		UniversityID  string `json:"university_id" format:"uuid"`
		ClubID        string `json:"club_id,omitempty" format:"uuid" doc:"The club whose own page this is"`
		IntervalHours int    `json:"interval_hours,omitempty" minimum:"0" maximum:"720" doc:"0: the kind's default (lists 24, events 6)"`
		Enabled       *bool  `json:"enabled,omitempty" doc:"Default: the environment's (INGEST_SOURCE_DEFAULT)"`
	}
}

func (h handler) adminCreateSource(ctx context.Context, in *adminCreateSourceInput) (*sourceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	club, err := httpx.OptID("body.club_id", in.Body.ClubID)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminCreateSource(ctx, a, SourceInput{
		Kind: Kind(in.Body.Kind), Adapter: AdapterName(in.Body.Adapter), URL: in.Body.URL, Label: in.Body.Label,
		UniversityID: httpx.ParseID(in.Body.UniversityID), ClubOrgID: club,
		Interval: time.Duration(in.Body.IntervalHours) * time.Hour, Enabled: in.Body.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return &sourceOutput{Body: toSourceDetail(v)}, nil
}

type adminAddPilotInput struct {
	Body struct {
		Pilot        string `json:"pilot" enum:"ytu,boun,sabanci,metu,itu"`
		UniversityID string `json:"university_id" format:"uuid"`
	}
}

type pilotOutput struct{ Body IngestPilotResultDTO }

func (h handler) adminAddPilot(ctx context.Context, in *adminAddPilotInput) (*pilotOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.AdminAddPilotSources(ctx, a, PilotInput{Pilot: in.Body.Pilot, UniversityID: httpx.ParseID(in.Body.UniversityID)})
	if err != nil {
		return nil, err
	}
	out := &pilotOutput{Body: IngestPilotResultDTO{Created: r.Created, Existing: r.Existing, Sources: make([]IngestSourceDTO, len(r.Sources))}}
	for i, s := range r.Sources {
		out.Body.Sources[i] = toSourceDTO(s)
	}
	return out, nil
}

type adminUpdateSourceInput struct {
	SourcePath
	Body struct {
		Enabled       *bool `json:"enabled,omitempty"`
		IntervalHours *int  `json:"interval_hours,omitempty" minimum:"1" maximum:"720"`
	}
}

func (h handler) adminUpdateSource(ctx context.Context, in *adminUpdateSourceInput) (*sourceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p := SourcePatch{Enabled: in.Body.Enabled}
	if in.Body.IntervalHours != nil {
		d := time.Duration(*in.Body.IntervalHours) * time.Hour
		p.Interval = &d
	}
	v, err := h.svc.AdminUpdateSource(ctx, a, httpx.ParseID(in.SourceID), p)
	if err != nil {
		return nil, err
	}
	return &sourceOutput{Body: toSourceDetail(v)}, nil
}

func (h handler) adminRunSource(ctx context.Context, in *SourcePath) (*sourceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminRunSource(ctx, a, httpx.ParseID(in.SourceID))
	if err != nil {
		return nil, err
	}
	return &sourceOutput{Body: toSourceDetail(v)}, nil
}

// ---- admin: imported events and clubs

type adminListEventsInput struct {
	page.Params
	Status       string `query:"status" enum:"imported,hidden,promoted,rejected"`
	UniversityID string `query:"university_id" format:"uuid"`
	ClubID       string `query:"club_id" format:"uuid"`
	SourceID     string `query:"source_id" format:"uuid"`
}

type adminEventListOutput struct{ Body AdminImportedEventList }

func (h handler) adminListEvents(ctx context.Context, in *adminListEventsInput) (*adminEventListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	f := EventFilter{}
	if in.Status != "" {
		f.Statuses = []Status{Status(in.Status)}
	}
	if f.UniversityID, err = httpx.OptID("query.university_id", in.UniversityID); err != nil {
		return nil, err
	}
	if f.ClubOrgID, err = httpx.OptID("query.club_id", in.ClubID); err != nil {
		return nil, err
	}
	if f.SourceID, err = httpx.OptID("query.source_id", in.SourceID); err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListImportedEvents", in.Status, in.UniversityID, in.ClubID, in.SourceID)
	after, err := eventCursor(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListImportedEvents(ctx, a, AdminEventQuery{Filter: f, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &adminEventListOutput{Body: AdminImportedEventList{Items: make([]AdminImportedEventDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toAdminEventDTO(v)
	}
	out.Body.NextCursor = nextEventCursor(p.Next, fp)
	return out, nil
}

type adminSetStatusInput struct {
	ImportedEventPath
	Body struct {
		Status string `json:"status" enum:"hidden,imported" doc:"imported restores a hidden event"`
	}
}

type adminEventOutput struct{ Body AdminImportedEventDTO }

func (h handler) adminSetStatus(ctx context.Context, in *adminSetStatusInput) (*adminEventOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminSetEventStatus(ctx, a, httpx.ParseID(in.ImportedEventID), Status(in.Body.Status))
	if err != nil {
		return nil, err
	}
	return &adminEventOutput{Body: toAdminEventDTO(v)}, nil
}

func (h handler) adminDeleteEvent(ctx context.Context, in *ImportedEventPath) (*noContent, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.AdminDeleteImportedEvent(ctx, a, httpx.ParseID(in.ImportedEventID)); err != nil {
		return nil, err
	}
	return &noContent{}, nil
}

type adminListClubsInput struct {
	page.Params
	UniversityID string `query:"university_id" format:"uuid"`
	NeedsReview  string `query:"needs_review" enum:"true,false"`
}

type clubListOutput struct{ Body ImportedClubList }

func (h handler) adminListClubs(ctx context.Context, in *adminListClubsInput) (*clubListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	uni, err := httpx.OptID("query.university_id", in.UniversityID)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListImportedClubs", in.UniversityID, in.NeedsReview)
	after, err := idCursor(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListImportedClubs(ctx, a, ClubQuery{
		Filter: ImportedClubFilter{UniversityID: uni, NeedsReview: optBool(in.NeedsReview)}, Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	out := &clubListOutput{Body: ImportedClubList{Items: make([]ImportedClubDTO, len(p.Items))}}
	for i, c := range p.Items {
		out.Body.Items[i] = toClubDTO(c)
	}
	out.Body.NextCursor = nextIDCursor(p.Next, fp)
	return out, nil
}

func (h handler) adminMarkReviewed(ctx context.Context, in *ClubPath) (*noContent, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.AdminMarkClubReviewed(ctx, a, httpx.ParseID(in.ClubID)); err != nil {
		return nil, err
	}
	return &noContent{}, nil
}
