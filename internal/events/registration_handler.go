package events

import (
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// registerRegistrations adds the 8 registration operations of BE-028 (04
// §2.6, §E.2, §E.6, §V.6).
func registerRegistrations(api huma.API, h handler) {
	reg(api, huma.Operation{
		OperationID: "createRegistration", Method: http.MethodPost, Path: "/events/{event_id}/registrations",
		DefaultStatus: http.StatusCreated,
		Summary:       "Register for an event",
		Description: "Verified students only (403 students.verification_required). The answers are validated against " +
			"the current form version (form_version_id of an older version: 409 events.form_version_stale; field " +
			"errors: 422 at body.answers.<key>). Every consent field answered true needs its consent text proof in " +
			"consents. auto mode: confirmed (with a ticket) or, when full, waitlisted (waiting list open) or 409 " +
			"events.capacity_full; manual mode: pending. One live registration per user (409 " +
			"events.already_registered). Outside the registration window: 409 events.registration_closed. " +
			"Idempotency-Key is required: a retry with the same key and body replays the first answer.",
	}, op.Meta{Action: string(authz.EventsRegistrationCreate), Surface: op.SurfaceResource, Idempotency: op.IdempotencyRequired},
		h.createRegistration)

	reg(api, huma.Operation{
		OperationID: "listMyRegistrations", Method: http.MethodGet, Path: "/me/registrations",
		Summary:     "List my registrations",
		Description: "The caller's registrations by the event's start (no answers); upcoming=true keeps events that have not ended.",
	}, op.Meta{Action: string(authz.EventsRegistrationListOwn), Surface: op.SurfaceSelf}, h.listMyRegistrations)

	reg(api, huma.Operation{
		OperationID: "getMyRegistration", Method: http.MethodGet, Path: "/me/registrations/{registration_id}",
		Summary:     "Get my registration",
		Description: "With the answers; the special-category answers are opened for the owner (the read is audited).",
	}, op.Meta{Action: string(authz.EventsRegistrationReadOwn), Surface: op.SurfaceSelf}, h.getMyRegistration)

	reg(api, huma.Operation{
		OperationID: "cancelMyRegistration", Method: http.MethodPost, Path: "/me/registrations/{registration_id}/cancel",
		Summary: "Cancel my registration",
		Description: "A confirmed registration is cancelled until cancel_deadline_at and the event's start (409 " +
			"events.registration_closed after), its ticket is revoked and the first on the waiting list takes the " +
			"seat; a pending or waitlisted one is withdrawn.",
	}, op.Meta{Action: string(authz.EventsRegistrationCancelOwn), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional},
		h.cancelMyRegistration)

	reg(api, huma.Operation{
		OperationID: "listEventRegistrations", Method: http.MethodGet, Path: "/events/{event_id}/registrations",
		Summary: "List an event's registrations",
		Description: "The organiser's managers; support and platform_admin by break-glass (X-Koro-Access-Reason, " +
			"10–500 characters; never a sensitive event). The special-category answers are never in the list. Every " +
			"list is audited.",
	}, op.Meta{Action: string(authz.EventsRegistrationList), Surface: op.SurfaceResource, BreakGlass: true},
		h.listEventRegistrations)

	reg(api, huma.Operation{
		OperationID: "getEventRegistration", Method: http.MethodGet, Path: "/events/{event_id}/registrations/{registration_id}",
		Summary: "Get an event registration",
		Description: "answers_sensitive (special-category answers) for the organiser's managers only, never by " +
			"break-glass. Every read is audited (metadata.sensitive_fields).",
	}, op.Meta{Action: string(authz.EventsRegistrationRead), Surface: op.SurfaceResource, BreakGlass: true},
		h.getEventRegistration)

	reg(api, huma.Operation{
		OperationID: "decideEventRegistration", Method: http.MethodPost,
		Path:    "/events/{event_id}/registrations/{registration_id}/decision",
		Summary: "Decide a pending application",
		Description: "manual mode, pending registrations only (409 events.invalid_state otherwise). confirmed takes a " +
			"seat (409 events.capacity_full) and issues the ticket; waitlisted needs the waiting list; the note is " +
			"never shown to the student.",
	}, op.Meta{Action: string(authz.EventsRegistrationDecide), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.decideRegistration)

	reg(api, huma.Operation{
		OperationID: "cancelEventRegistration", Method: http.MethodPost,
		Path:    "/events/{event_id}/registrations/{registration_id}/cancel",
		Summary: "Cancel a registration on behalf of the organiser",
		Description: "pending, waitlisted or confirmed → cancelled (cancelled_by_kind org); a confirmed seat goes to " +
			"the waiting list.",
	}, op.Meta{Action: string(authz.EventsRegistrationCancel), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.cancelEventRegistration)
}

// ---- DTOs

// RegistrationEventDTO is the event of a registration (04 §E.2).
type RegistrationEventDTO struct {
	EventID         string    `json:"event_id" format:"uuid"`
	Title           string    `json:"title"`
	Status          string    `json:"status" enum:"draft,published,cancelled,completed"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	DisplayTimezone string    `json:"display_timezone" enum:"Europe/Istanbul"`
	LocationName    string    `json:"location_name,omitempty"`
}

// RegistrationSelfDTO is the registrant's view (04 §E.2, getMyRegistration
// adds the answers).
type RegistrationSelfDTO struct {
	RegistrationID   string               `json:"registration_id" format:"uuid"`
	Event            RegistrationEventDTO `json:"event"`
	Status           string               `json:"status" enum:"pending,confirmed,waitlisted,rejected,cancelled,checked_in,no_show,expired,withdrawn"`
	FormVersionID    string               `json:"form_version_id,omitempty" format:"uuid"`
	CreatedAt        time.Time            `json:"created_at"`
	ConfirmedAt      *time.Time           `json:"confirmed_at,omitempty"`
	WaitlistedAt     *time.Time           `json:"waitlisted_at,omitempty"`
	CancelledAt      *time.Time           `json:"cancelled_at,omitempty"`
	WaitlistPosition *int                 `json:"waitlist_position" doc:"Current place on the waiting list (an estimate); null when not waitlisted"`
	TicketAvailable  bool                 `json:"ticket_available"`
	Answers          map[string]any       `json:"answers,omitempty" doc:"getMyRegistration only"`
	AnswersSensitive map[string]any       `json:"answers_sensitive,omitempty" doc:"Special-category answers (getMyRegistration only)"`
	ConsentSnapshot  map[string]bool      `json:"consent_snapshot,omitempty" doc:"Consent fields answered (purpose → answer)"`
}

// RegistrationSelfList is a page of the caller's registrations.
type RegistrationSelfList struct {
	Items      []RegistrationSelfDTO `json:"items"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// NamedRefDTO is a named reference (university, department).
type NamedRefDTO struct {
	Name string `json:"name"`
}

// RegistrantDTO is the registrant on the organiser's views (04 §V.6): no
// e-mail, phone, birth date, other registrations or talent.
type RegistrantDTO struct {
	UserID     string       `json:"user_id" format:"uuid"`
	FirstName  string       `json:"first_name"`
	LastName   string       `json:"last_name"`
	AvatarURL  string       `json:"avatar_url,omitempty" doc:"Short-lived URL"`
	University *NamedRefDTO `json:"university,omitempty"`
	Department *NamedRefDTO `json:"department,omitempty"`
	ClassYear  *int16       `json:"class_year,omitempty"`
}

// RegistrationOrgDTO is the organiser's view of a registration (04 §V.6).
type RegistrationOrgDTO struct {
	RegistrationID   string          `json:"registration_id" format:"uuid"`
	Status           string          `json:"status" enum:"pending,confirmed,waitlisted,rejected,cancelled,checked_in,no_show,expired,withdrawn"`
	Source           string          `json:"source" enum:"web,mobile,admin"`
	CreatedAt        time.Time       `json:"created_at"`
	ConfirmedAt      *time.Time      `json:"confirmed_at,omitempty"`
	WaitlistedAt     *time.Time      `json:"waitlisted_at,omitempty"`
	CheckedInAt      *time.Time      `json:"checked_in_at,omitempty"`
	CancelledAt      *time.Time      `json:"cancelled_at,omitempty"`
	User             RegistrantDTO   `json:"user"`
	FormVersionID    string          `json:"form_version_id,omitempty" format:"uuid"`
	Answers          map[string]any  `json:"answers" doc:"Answers without the special-category fields"`
	AnswersSensitive map[string]any  `json:"answers_sensitive,omitempty" doc:"getEventRegistration for the organiser's managers only (audited)"`
	ConsentSnapshot  map[string]bool `json:"consent_snapshot"`
	DecisionNote     string          `json:"decision_note,omitempty" doc:"Managers only; never shown to the student"`
}

// RegistrationOrgList is a page of an event's registrations.
type RegistrationOrgList struct {
	Items      []RegistrationOrgDTO `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func toSelfDTO(v RegistrationView) RegistrationSelfDTO {
	r, e := v.Registration, v.Event
	d := RegistrationSelfDTO{
		RegistrationID: r.ID.String(), Status: string(r.Status), CreatedAt: r.CreatedAt, ConfirmedAt: r.ConfirmedAt,
		WaitlistedAt: r.WaitlistedAt, CancelledAt: r.CancelledAt, WaitlistPosition: v.WaitlistPosition,
		TicketAvailable: v.TicketAvailable, Answers: v.Answers, AnswersSensitive: v.AnswersSensitive,
		Event: RegistrationEventDTO{EventID: e.ID.String(), Title: e.Title, Status: string(e.Status), StartsAt: e.StartsAt,
			EndsAt: e.EndsAt, DisplayTimezone: displayTimezone, LocationName: str(e.LocationName)},
	}
	if len(v.ConsentSnapshot) > 0 {
		d.ConsentSnapshot = v.ConsentSnapshot
	}
	if r.FormVersionID != nil {
		d.FormVersionID = r.FormVersionID.String()
	}
	return d
}

func toOrgDTO(v RegistrationView) RegistrationOrgDTO {
	r := v.Registration
	d := RegistrationOrgDTO{
		RegistrationID: r.ID.String(), Status: string(r.Status), Source: string(r.Source), CreatedAt: r.CreatedAt,
		ConfirmedAt: r.ConfirmedAt, WaitlistedAt: r.WaitlistedAt, CheckedInAt: r.CheckedInAt, CancelledAt: r.CancelledAt,
		Answers: v.Answers, AnswersSensitive: v.AnswersSensitive, ConsentSnapshot: v.ConsentSnapshot,
	}
	if d.Answers == nil {
		d.Answers = map[string]any{}
	}
	if d.ConsentSnapshot == nil {
		d.ConsentSnapshot = map[string]bool{}
	}
	if r.FormVersionID != nil {
		d.FormVersionID = r.FormVersionID.String()
	}
	if v.ShowNote && r.DecisionNote != nil {
		d.DecisionNote = *r.DecisionNote
	}
	if u := v.User; u != nil {
		d.User = RegistrantDTO{UserID: u.UserID.String(), FirstName: u.FirstName, LastName: u.LastName, AvatarURL: u.AvatarURL,
			ClassYear: u.ClassYear}
		if u.UniversityName != "" {
			d.User.University = &NamedRefDTO{Name: u.UniversityName}
		}
		if u.DepartmentName != "" {
			d.User.Department = &NamedRefDTO{Name: u.DepartmentName}
		}
	} else {
		d.User = RegistrantDTO{UserID: r.UserID.String()}
	}
	return d
}

// ---- paths

// RegistrationPath is the registration_id path parameter of /me.
type RegistrationPath struct {
	RegistrationID string `path:"registration_id" doc:"Registration ID"`
}

func (p RegistrationPath) id() uuid.UUID { return parseID(p.RegistrationID) }

// EventRegistrationPath is /events/{event_id}/registrations/{registration_id}.
type EventRegistrationPath struct {
	EventPath
	RegistrationID string `path:"registration_id" doc:"Registration ID"`
}

func (p EventRegistrationPath) regID() uuid.UUID { return parseID(p.RegistrationID) }

// AccessHeaders are the break-glass headers of the organiser's reads (03 §6.3).
type AccessHeaders struct {
	AccessReason string `header:"X-Koro-Access-Reason" maxLength:"500" doc:"Break-glass justification (support, platform_admin: 10–500 characters)"`
	TicketRef    string `header:"X-Koro-Ticket-Ref" maxLength:"100" doc:"Support ticket reference (optional)"`
}

func (a AccessHeaders) access() Access {
	return Access{Reason: strings.TrimSpace(a.AccessReason), TicketRef: strings.TrimSpace(a.TicketRef)}
}

// ---- createRegistration

// ConsentProofBody proves the consent text shown for a consent field.
type ConsentProofBody struct {
	PurposeCode     string `json:"purpose_code" maxLength:"64" doc:"The consent purpose of the field (event_sponsor_share)"`
	NoticeVersionID string `json:"notice_version_id" format:"uuid" doc:"consent_notices[].notice_version_id of getEventForm"`
	TextSHA256      string `json:"text_sha256" pattern:"^[0-9a-f]{64}$" doc:"consent_notices[].body_sha256 of getEventForm"`
}

type createRegistrationInput struct {
	EventPath
	idempotency.RequiredHeader
	Client string `header:"X-Koro-Client" enum:"web,mobile" doc:"Client kind; the registration source and the consent channel (default web)"`
	Body   struct {
		FormVersionID string             `json:"form_version_id,omitempty" format:"uuid" doc:"The form version answered (getEventForm); default: the current one"`
		Answers       map[string]any     `json:"answers,omitempty" doc:"Answers by field key (see getEventForm answers_schema)"`
		Consents      []ConsentProofBody `json:"consents,omitempty" maxItems:"10" doc:"One proof per consent field answered true"`
	}
}

type selfOutput struct {
	Body RegistrationSelfDTO
}

func (h handler) createRegistration(ctx context.Context, in *createRegistrationInput) (*selfOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ri := RegistrationInput{Answers: in.Body.Answers, Source: Source(in.Client)}
	if in.Body.FormVersionID != "" {
		id, err := uuid.Parse(in.Body.FormVersionID)
		if err != nil {
			return nil, fieldErr("body.form_version_id", "must be a UUID")
		}
		ri.FormVersionID = &id
	}
	for i, c := range in.Body.Consents {
		loc := "body.consents[" + strconv.Itoa(i) + "]"
		nid, err := uuid.Parse(c.NoticeVersionID)
		if err != nil {
			return nil, fieldErr(loc+".notice_version_id", "must be a UUID")
		}
		sum, err := hex.DecodeString(c.TextSHA256)
		if err != nil || len(sum) != 32 {
			return nil, fieldErr(loc+".text_sha256", "must be a hex SHA-256 digest")
		}
		ri.Consents = append(ri.Consents, ConsentProof{Purpose: consent.Purpose(c.PurposeCode), NoticeVersionID: nid, TextSHA256: sum})
	}
	v, err := h.svc.CreateRegistration(ctx, a, in.id(), ri)
	if err != nil {
		return nil, err
	}
	return &selfOutput{Body: toSelfDTO(v)}, nil
}

// ---- listMyRegistrations

type listMyRegistrationsInput struct {
	page.Params
	Status   []string `query:"status,explode" maxItems:"9" enum:"pending,confirmed,waitlisted,rejected,cancelled,checked_in,no_show,expired,withdrawn" doc:"Statuses (repeat the parameter)"`
	Upcoming bool     `query:"upcoming" doc:"Only events that have not ended"`
	Sort     string   `query:"sort" enum:"starts_at" doc:"starts_at (the only order)"`
}

type selfListOutput struct {
	Body RegistrationSelfList
}

func regStatuses(raw []string) []RegStatus {
	out := make([]RegStatus, 0, len(raw))
	for _, s := range raw {
		out = append(out, RegStatus(s))
	}
	return out
}

func (h handler) listMyRegistrations(ctx context.Context, in *listMyRegistrationsInput) (*selfListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q := MyRegistrationQuery{Statuses: regStatuses(in.Status), Upcoming: in.Upcoming, Limit: in.Limit}
	fp := page.Fingerprint("listMyRegistrations", strings.Join(in.Status, ","), strconv.FormatBool(in.Upcoming))
	if q.After, err = cursorOf(in.Cursor, SortStartsAt, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListMyRegistrations(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &selfListOutput{Body: RegistrationSelfList{Items: make([]RegistrationSelfDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toSelfDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(SortStartsAt, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// ---- getMyRegistration

func (h handler) getMyRegistration(ctx context.Context, in *RegistrationPath) (*selfOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetMyRegistration(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return &selfOutput{Body: toSelfDTO(v)}, nil
}

// ---- cancelMyRegistration

type cancelMyRegistrationInput struct {
	RegistrationPath
	Body *struct {
		Reason string `json:"reason,omitempty" maxLength:"300" doc:"Optional reason"`
	}
}

func (h handler) cancelMyRegistration(ctx context.Context, in *cancelMyRegistrationInput) (*selfOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var reason string
	if in.Body != nil {
		reason = in.Body.Reason
	}
	v, err := h.svc.CancelMyRegistration(ctx, a, in.id(), reason)
	if err != nil {
		return nil, err
	}
	return &selfOutput{Body: toSelfDTO(v)}, nil
}

// ---- listEventRegistrations

// sortCreatedNew is the newest-first order of listEventRegistrations.
const (
	sortCreated    = "created_at"
	sortCreatedNew = "-created_at"
)

type listEventRegistrationsInput struct {
	EventPath
	AccessHeaders
	page.Params
	Status []string `query:"status,explode" maxItems:"9" enum:"pending,confirmed,waitlisted,rejected,cancelled,checked_in,no_show,expired,withdrawn" doc:"Statuses (repeat the parameter)"`
	Sort   string   `query:"sort" enum:"created_at,-created_at" doc:"created_at (default, oldest first) or -created_at"`
}

type orgListOutput struct {
	Body RegistrationOrgList
}

func (h handler) listEventRegistrations(ctx context.Context, in *listEventRegistrationsInput) (*orgListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	sort := in.Sort
	if sort == "" {
		sort = sortCreated
	}
	q := EventRegistrationQuery{Statuses: regStatuses(in.Status), Desc: sort == sortCreatedNew, Limit: in.Limit}
	fp := page.Fingerprint("listEventRegistrations", in.EventID, strings.Join(in.Status, ","))
	if q.After, err = cursorOf(in.Cursor, sort, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListEventRegistrations(ctx, a, in.id(), q, in.access())
	if err != nil {
		return nil, err
	}
	out := &orgListOutput{Body: RegistrationOrgList{Items: make([]RegistrationOrgDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toOrgDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sort, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// ---- getEventRegistration

type getEventRegistrationInput struct {
	EventRegistrationPath
	AccessHeaders
}

type orgOutput struct {
	Body RegistrationOrgDTO
}

func (h handler) getEventRegistration(ctx context.Context, in *getEventRegistrationInput) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetEventRegistration(ctx, a, in.id(), in.regID(), in.access())
	if err != nil {
		return nil, err
	}
	return &orgOutput{Body: toOrgDTO(v)}, nil
}

// ---- decideEventRegistration

type decideRegistrationInput struct {
	EventRegistrationPath
	Body struct {
		Decision     string `json:"decision" enum:"confirmed,rejected,waitlisted"`
		DecisionNote string `json:"decision_note,omitempty" maxLength:"500" doc:"For the organiser only; never shown to the student"`
	}
}

func (h handler) decideRegistration(ctx context.Context, in *decideRegistrationInput) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.DecideRegistration(ctx, a, in.id(), in.regID(), Decision{Status: RegStatus(in.Body.Decision), Note: in.Body.DecisionNote})
	if err != nil {
		return nil, err
	}
	return &orgOutput{Body: toOrgDTO(v)}, nil
}

// ---- cancelEventRegistration

type cancelEventRegistrationInput struct {
	EventRegistrationPath
	Body struct {
		CancelReason string `json:"cancel_reason" maxLength:"300" doc:"3-300 characters"`
	}
}

func (h handler) cancelEventRegistration(ctx context.Context, in *cancelEventRegistrationInput) (*orgOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CancelEventRegistration(ctx, a, in.id(), in.regID(), in.Body.CancelReason)
	if err != nil {
		return nil, err
	}
	return &orgOutput{Body: toOrgDTO(v)}, nil
}
