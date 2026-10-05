package events

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// registerCheckins adds the 9 ticket, check-in and attendance operations
// of BE-031 (04 §2.6, §E.3, §E.5).
func registerCheckins(api huma.API, h handler) {
	reg(api, huma.Operation{
		OperationID: "getMyTicket", Method: http.MethodGet, Path: "/me/registrations/{registration_id}/ticket",
		Summary: "Get my ticket",
		Description: "The QR payload (KORO1:<token>; no personal data, no URL) and the manual code of a confirmed or " +
			"checked-in registration's active ticket; 409 events.invalid_state for any other registration. The payload " +
			"is the same as in the ticket e-mail: the app may keep it to show the QR offline. Treat it as a secret.",
	}, op.Meta{Action: string(authz.EventsTicketReadOwn), Surface: op.SurfaceSelf}, h.getMyTicket)

	reg(api, huma.Operation{
		OperationID: "resendMyTicket", Method: http.MethodPost, Path: "/me/registrations/{registration_id}/ticket/resend",
		DefaultStatus: http.StatusAccepted,
		Summary:       "E-mail my ticket again",
		Description: "The same ticket (the same QR) is e-mailed again from the worker. At most 3 per hour " +
			"(ticket_resend, 429). 409 events.invalid_state without an active ticket; 503 when the mail queue is down.",
	}, op.Meta{Action: string(authz.EventsTicketResend), Surface: op.SurfaceSelf, RateLimit: op.RateTicketResend,
		Idempotency: op.IdempotencyOptional}, h.resendMyTicket)

	reg(api, huma.Operation{
		OperationID: "reissueMyTicket", Method: http.MethodPost, Path: "/me/registrations/{registration_id}/ticket/reissue",
		Summary: "Reissue my ticket",
		Description: "A new ticket (version + 1) replaces the active one: the old QR and manual code stop working " +
			"(revoked), the new ticket is e-mailed. Confirmed registrations of a published event only (409 " +
			"events.invalid_state).",
	}, op.Meta{Action: string(authz.EventsTicketReissue), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional},
		h.reissueMyTicket)

	reg(api, huma.Operation{
		OperationID: "reissueRegistrationTicket", Method: http.MethodPost,
		Path:    "/events/{event_id}/registrations/{registration_id}/ticket/reissue",
		Summary: "Reissue a registration's ticket",
		Description: "The organiser's managers replace a confirmed registration's ticket (version + 1, the old QR " +
			"revoked). The token is never returned: the registrant gets the new ticket by e-mail.",
	}, op.Meta{Action: string(authz.EventsTicketReissue), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.reissueRegistrationTicket)

	reg(api, huma.Operation{
		OperationID: "createCheckin", Method: http.MethodPost, Path: "/events/{event_id}/check-ins",
		Summary: "Check an attendee in",
		Description: "The event's staff and the organiser's managers scan a QR (method qr, token = the scanned " +
			"KORO1:… text) or type a manual code. Business outcomes are 200 with result: ok, already_checked_in (with " +
			"previous), not_confirmed, revoked, wrong_event (a ticket of another event) and not_found; wrong_event and " +
			"not_found never carry the attendee. A retry with the same client_scan_id returns the first answer; a " +
			"client_scan_id of another scan is 409. Check-in must be enabled, the event published and its attendance " +
			"open (409 events.invalid_state). Idempotency-Key is required; at most 120 scans per minute (qr_scan).",
	}, op.Meta{Action: string(authz.EventsCheckinCreate), Surface: op.SurfaceResource, RateLimit: op.RateQRScan,
		Idempotency: op.IdempotencyRequired}, h.createCheckin)

	reg(api, huma.Operation{
		OperationID: "voidCheckin", Method: http.MethodPost, Path: "/events/{event_id}/check-ins/{checkin_id}/void",
		Summary: "Void a check-in",
		Description: "A manager (staff role manager or the organiser's managers) voids a check-in with a reason; the " +
			"registration is confirmed again. A voided check-in or a closed attendance is 409 events.invalid_state.",
	}, op.Meta{Action: string(authz.EventsCheckinVoid), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.voidCheckin)

	reg(api, huma.Operation{
		OperationID: "listCheckins", Method: http.MethodGet, Path: "/events/{event_id}/check-ins",
		Summary:     "List an event's check-ins",
		Description: "The latest scan first; the attendee as first name and last-name initial only.",
	}, op.Meta{Action: string(authz.EventsCheckinList), Surface: op.SurfaceResource}, h.listCheckins)

	reg(api, huma.Operation{
		OperationID: "getCheckinStats", Method: http.MethodGet, Path: "/events/{event_id}/check-ins/stats",
		Summary: "Get an event's attendance counters",
		Description: "confirmed: the seat holders (confirmed and checked in); checked_in; voided check-ins; " +
			"waitlisted; no_show after the attendance is closed. The live board is a WebSocket (later).",
	}, op.Meta{Action: string(authz.EventsCheckinStats), Surface: op.SurfaceResource}, h.getCheckinStats)

	reg(api, huma.Operation{
		OperationID: "finalizeEventAttendance", Method: http.MethodPost, Path: "/events/{event_id}/attendance/finalize",
		Summary: "Close an event's attendance",
		Description: "After the event's end, once: the confirmed registrations without a check-in become no_show " +
			"(in the background); check-ins and voids stop. Before the end, a second time or without check-in: 409 " +
			"events.invalid_state. Otherwise the system closes it 24 hours after the end.",
	}, op.Meta{Action: string(authz.EventsAttendanceFinalize), Surface: op.SurfaceResource,
		Idempotency: op.IdempotencyOptional}, h.finalizeAttendance)
}

// ---- DTOs

// TicketDTO is the registrant's ticket (04 §E.3).
type TicketDTO struct {
	TicketID   string       `json:"ticket_id" format:"uuid"`
	QRPayload  string       `json:"qr_payload" doc:"KORO1:<token>: render it as a QR code; a bearer credential"`
	ManualCode string       `json:"manual_code" doc:"8 characters (Crockford base32) for the door when the QR cannot be read"`
	Version    int          `json:"version"`
	IssuedAt   time.Time    `json:"issued_at"`
	Event      EventCardDTO `json:"event"`
}

func toTicketDTO(v TicketView) TicketDTO {
	return TicketDTO{TicketID: v.Ticket.ID.String(), QRPayload: v.Payload, ManualCode: v.Ticket.ManualCode,
		Version: v.Ticket.Version, IssuedAt: v.Ticket.IssuedAt, Event: toCardDTO(v.Event)}
}

// AttendeeDTO is the attendee of a scan ("Ayşe K.").
type AttendeeDTO struct {
	AttendeeDisplay string `json:"attendee_display"`
}

// PreviousCheckinDTO is the first check-in of an already_checked_in scan
// (null otherwise: the schema is nullable, 04 §E.5 "previous": null).
type PreviousCheckinDTO struct {
	_            struct{}  `nullable:"true"`
	CheckedInAt  time.Time `json:"checked_in_at"`
	StaffDisplay string    `json:"staff_display" doc:"Who scanned it (first name and last-name initial)"`
}

// CheckinResultDTO is the answer of createCheckin (04 §E.5).
type CheckinResultDTO struct {
	Result      string              `json:"result" enum:"ok,already_checked_in,not_confirmed,revoked,wrong_event,not_found"`
	CheckinID   string              `json:"checkin_id,omitempty" format:"uuid"`
	Attendee    *AttendeeDTO        `json:"attendee,omitempty" doc:"Absent for wrong_event and not_found"`
	CheckedInAt *time.Time          `json:"checked_in_at,omitempty"`
	Previous    *PreviousCheckinDTO `json:"previous" doc:"The first check-in (already_checked_in); null otherwise"`
}

func toCheckinResultDTO(r CheckinResult) CheckinResultDTO {
	d := CheckinResultDTO{Result: string(r.Result), CheckedInAt: r.CheckedInAt}
	if r.CheckinID != nil {
		d.CheckinID = r.CheckinID.String()
	}
	if r.Attendee != "" && r.Result != ResultWrongEvent && r.Result != ResultNotFound {
		d.Attendee = &AttendeeDTO{AttendeeDisplay: r.Attendee}
	}
	if p := r.Previous; p != nil {
		d.Previous = &PreviousCheckinDTO{CheckedInAt: p.CheckedInAt, StaffDisplay: p.StaffDisplay}
	}
	return d
}

// CheckinVoidDTO is the answer of voidCheckin (04 §E.5).
type CheckinVoidDTO struct {
	CheckinID          string    `json:"checkin_id" format:"uuid"`
	VoidedAt           time.Time `json:"voided_at"`
	RegistrationStatus string    `json:"registration_status" enum:"confirmed"`
}

// CheckinItemDTO is one row of listCheckins (04 §2.6).
type CheckinItemDTO struct {
	CheckinID       string    `json:"checkin_id" format:"uuid"`
	AttendeeDisplay string    `json:"attendee_display"`
	Method          string    `json:"method" enum:"qr,manual_code"`
	ScannedAt       time.Time `json:"scanned_at"`
	Voided          bool      `json:"voided"`
}

// CheckinList is a page of check-ins.
type CheckinList struct {
	Items      []CheckinItemDTO `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// CheckinStatsDTO is the answer of getCheckinStats (04 §2.6).
type CheckinStatsDTO struct {
	Confirmed  int       `json:"confirmed" doc:"Seat holders: confirmed and checked-in registrations"`
	CheckedIn  int       `json:"checked_in"`
	Voided     int       `json:"voided" doc:"Voided check-ins"`
	Waitlisted int       `json:"waitlisted"`
	NoShow     int       `json:"no_show" doc:"After the attendance is closed"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ---- paths

// CheckinPath is /events/{event_id}/check-ins/{checkin_id}.
type CheckinPath struct {
	EventPath
	CheckinID string `path:"checkin_id" doc:"Check-in ID"`
}

func (p CheckinPath) checkinID() uuid.UUID { return parseID(p.CheckinID) }

// ---- tickets

type ticketOutput struct {
	Body TicketDTO
}

func (h handler) getMyTicket(ctx context.Context, in *RegistrationPath) (*ticketOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetMyTicket(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return &ticketOutput{Body: toTicketDTO(v)}, nil
}

type resendTicketInput struct {
	RegistrationPath
	idempotency.OptionalHeader
}

func (h handler) resendMyTicket(ctx context.Context, in *resendTicketInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.ResendMyTicket(ctx, a, in.id()); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

type reissueMyTicketInput struct {
	RegistrationPath
	idempotency.OptionalHeader
}

func (h handler) reissueMyTicket(ctx context.Context, in *reissueMyTicketInput) (*ticketOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReissueMyTicket(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return &ticketOutput{Body: toTicketDTO(v)}, nil
}

type reissueRegistrationTicketInput struct {
	EventRegistrationPath
	idempotency.OptionalHeader
}

type reissuedOutput struct {
	Body struct {
		Version int `json:"version" doc:"The new ticket version (the token is mailed to the registrant)"`
	}
}

func (h handler) reissueRegistrationTicket(ctx context.Context, in *reissueRegistrationTicketInput) (*reissuedOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReissueRegistrationTicket(ctx, a, in.id(), in.regID())
	if err != nil {
		return nil, err
	}
	out := &reissuedOutput{}
	out.Body.Version = v
	return out, nil
}

// ---- check-in

type createCheckinInput struct {
	EventPath
	idempotency.RequiredHeader
	Body struct {
		Method       string    `json:"method" enum:"qr,manual_code"`
		Token        string    `json:"token,omitempty" maxLength:"64" doc:"The scanned QR text (KORO1:…), method qr"`
		ManualCode   string    `json:"manual_code,omitempty" maxLength:"16" doc:"The typed code, method manual_code"`
		ClientScanID string    `json:"client_scan_id" format:"uuid" doc:"The device's ID of this scan: a retry with the same ID returns the first answer"`
		ScannedAt    time.Time `json:"scanned_at" doc:"Device time of the scan (within the last 24 hours)"`
		DeviceID     string    `json:"device_id,omitempty" maxLength:"100"`
	}
}

type checkinOutput struct {
	Body CheckinResultDTO
}

func (h handler) createCheckin(ctx context.Context, in *createCheckinInput) (*checkinOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	scan, err := uuid.Parse(in.Body.ClientScanID)
	if err != nil {
		return nil, fieldErr("body.client_scan_id", "must be a UUID")
	}
	r, err := h.svc.CreateCheckin(ctx, a, in.id(), CheckinInput{
		Method: CheckinMethod(in.Body.Method), Token: in.Body.Token, ManualCode: in.Body.ManualCode, ClientScanID: scan,
		ScannedAt: in.Body.ScannedAt, DeviceID: in.Body.DeviceID,
	})
	if err != nil {
		return nil, err
	}
	return &checkinOutput{Body: toCheckinResultDTO(r)}, nil
}

type voidCheckinInput struct {
	CheckinPath
	idempotency.OptionalHeader
	Body struct {
		VoidReason string `json:"void_reason" maxLength:"300" doc:"5-300 characters"`
	}
}

type voidOutput struct {
	Body CheckinVoidDTO
}

func (h handler) voidCheckin(ctx context.Context, in *voidCheckinInput) (*voidOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.VoidCheckin(ctx, a, in.id(), in.checkinID(), in.Body.VoidReason)
	if err != nil {
		return nil, err
	}
	out := &voidOutput{Body: CheckinVoidDTO{CheckinID: v.Checkin.ID.String(), RegistrationStatus: string(v.RegistrationStatus)}}
	if v.Checkin.VoidedAt != nil {
		out.Body.VoidedAt = *v.Checkin.VoidedAt
	}
	return out, nil
}

// sortScannedNew is the only order of listCheckins.
const sortScannedNew = "-scanned_at"

type listCheckinsInput struct {
	EventPath
	page.Params
	Sort string `query:"sort" enum:"-scanned_at" doc:"-scanned_at (the only order: the latest scan first)"`
}

type checkinListOutput struct {
	Body CheckinList
}

func (h handler) listCheckins(ctx context.Context, in *listCheckinsInput) (*checkinListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listCheckins", in.EventID)
	after, err := cursorOf(in.Cursor, sortScannedNew, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListCheckins(ctx, a, in.id(), in.Limit, after)
	if err != nil {
		return nil, err
	}
	out := &checkinListOutput{Body: CheckinList{Items: make([]CheckinItemDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = CheckinItemDTO{CheckinID: v.Checkin.ID.String(), AttendeeDisplay: v.Attendee,
			Method: string(v.Checkin.Method), ScannedAt: v.Checkin.ScannedAt, Voided: v.Checkin.VoidedAt != nil}
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortScannedNew, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

type statsOutput struct {
	Body CheckinStatsDTO
}

func (h handler) getCheckinStats(ctx context.Context, in *EventPath) (*statsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.svc.GetCheckinStats(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return &statsOutput{Body: CheckinStatsDTO{Confirmed: st.Confirmed, CheckedIn: st.CheckedIn, Voided: st.Voided,
		Waitlisted: st.Waitlisted, NoShow: st.NoShow, UpdatedAt: st.UpdatedAt}}, nil
}

type finalizeInput struct {
	EventPath
	idempotency.OptionalHeader
}

type finalizeOutput struct {
	Body struct {
		NoShowCount int `json:"no_show_count" doc:"Confirmed registrations without a check-in (marked no_show in the background)"`
	}
}

func (h handler) finalizeAttendance(ctx context.Context, in *finalizeInput) (*finalizeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	n, err := h.svc.FinalizeEventAttendance(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	out := &finalizeOutput{}
	out.Body.NoShowCount = n
	return out, nil
}
