package events

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// registerExports adds the 3 export operations of BE-050 (04 §2.6).
func registerExports(api huma.API, h handler) {
	op.Register(api, "events", huma.Operation{
		OperationID: "createEventExport", Method: http.MethodPost, Path: "/events/{event_id}/exports",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Request an export of an event's registrations",
		Description: "The organiser's managers (R: reauth with the password, or the TOTP code of an MFA account; " +
			"missing → 403 identity.reauth_required, wrong → 403 identity.reauth_failed). reason (10–500 characters) " +
			"and fields (1–40, distinct) are required: the registration columns registration_id, status, source, " +
			"created_at, confirmed_at, waitlisted_at, checked_in_at, cancelled_at, first_name, last_name, university, " +
			"department, class_year and answers.<key> for the non-sensitive fields of the event's form. There is no " +
			"e-mail column and special-category answers are never exported: include_sensitive=true is 422. The file " +
			"(xlsx by default, or csv) is built in the background and kept 7 days (expires_at). A suspended org " +
			"cannot export (403 orgs.org_suspended). Rate class export (5/hour).",
	}, op.Meta{Action: string(authz.EventsExportCreate), Surface: op.SurfaceResource, Reauth: true, RateLimit: op.RateExport,
		Idempotency: op.IdempotencyOptional}, h.createExport)

	op.Register(api, "events", huma.Operation{
		OperationID: "listEventExports", Method: http.MethodGet, Path: "/events/{event_id}/exports",
		Summary:     "List an event's exports",
		Description: "Newest first: who asked, why, which fields, the status, the row and download counts.",
	}, op.Meta{Action: string(authz.EventsExportList), Surface: op.SurfaceResource}, h.listExports)

	op.Register(api, "events", huma.Operation{
		OperationID: "downloadEventExport", Method: http.MethodGet, Path: "/events/{event_id}/exports/{export_id}/download",
		Summary: "Get a download link of an export",
		Description: "Only the manager who requested the export, only while it is ready and not expired (409 " +
			"events.export_not_ready otherwise). The presigned URL lives at most 5 minutes; every download is " +
			"counted and audited.",
	}, op.Meta{Action: string(authz.EventsExportDownload), Surface: op.SurfaceResource}, h.downloadExport)
}

// ExportDTO is an export (04 §2.6: createEventExport's ExportDTO; the
// list adds requested_by, row_count and download_count).
type ExportDTO struct {
	ExportID          string     `json:"export_id" format:"uuid"`
	Status            string     `json:"status" enum:"queued,ready,failed,purged"`
	Format            string     `json:"format" enum:"csv,xlsx"`
	Fields            []string   `json:"fields"`
	Reason            string     `json:"reason"`
	RequestedBy       string     `json:"requested_by" format:"uuid" doc:"User ID of the requester"`
	RowCount          *int       `json:"row_count" doc:"Rows of the file; null until it is ready"`
	DownloadCount     int        `json:"download_count"`
	FirstDownloadedAt *time.Time `json:"first_downloaded_at,omitempty"`
	ExpiresAt         time.Time  `json:"expires_at" doc:"The file is deleted at this time (7 days after the request)"`
	PurgedAt          *time.Time `json:"purged_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// ExportList is a page of an event's exports.
type ExportList struct {
	Items      []ExportDTO `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ExportURLDTO is a short-lived download URL (04 §1.14).
type ExportURLDTO struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func toExportDTO(x Export) ExportDTO {
	fields := []string(x.Fields)
	if fields == nil {
		fields = []string{}
	}
	return ExportDTO{
		ExportID: x.ID.String(), Status: string(x.Status), Format: string(x.Format), Fields: fields, Reason: x.Reason,
		RequestedBy: x.RequestedBy.String(), RowCount: x.RowCount, DownloadCount: x.DownloadCount,
		FirstDownloadedAt: x.FirstDownloadedAt, ExpiresAt: x.ExpiresAt, PurgedAt: x.PurgedAt, CreatedAt: x.CreatedAt,
	}
}

// ---- createEventExport

type createExportInput struct {
	EventPath
	idempotency.OptionalHeader
	Body struct {
		Reason           exportReasonText   `json:"reason" doc:"Why the data is needed"`
		Fields           exportFieldList    `json:"fields" doc:"Columns of the file (see the description)"`
		Format           string             `json:"format,omitempty" enum:"csv,xlsx" doc:"File type (default xlsx)"`
		IncludeSensitive bool               `json:"include_sensitive,omitempty" doc:"Not accepted in v1: true is 422 (special-category answers are never exported)"`
		Reauth           *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

type exportOutput struct {
	Body ExportDTO
}

func (h handler) createExport(ctx context.Context, in *createExportInput) (*exportOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var proof identity.ReauthProof
	if r := in.Body.Reauth; r != nil {
		proof = identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
	}
	x, err := h.svc.CreateExport(ctx, a, in.id(), ExportInput{Reason: string(in.Body.Reason), Fields: exportFields(in.Body.Fields),
		Format: ExportFormat(in.Body.Format), IncludeSensitive: in.Body.IncludeSensitive}, proof)
	if err != nil {
		return nil, err
	}
	return &exportOutput{Body: toExportDTO(x)}, nil
}

// ---- listEventExports

type listExportsInput struct {
	EventPath
	page.Params
}

type exportListOutput struct {
	Body ExportList
}

// sortExports is the only order of listEventExports.
const sortExports = "-created_at"

func (h handler) listExports(ctx context.Context, in *listExportsInput) (*exportListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listEventExports", in.EventID)
	after, err := cursorOf(in.Cursor, sortExports, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListExports(ctx, a, in.id(), in.Limit, after)
	if err != nil {
		return nil, err
	}
	out := &exportListOutput{Body: ExportList{Items: make([]ExportDTO, len(p.Items))}}
	for i, x := range p.Items {
		out.Body.Items[i] = toExportDTO(x)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortExports, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// ---- downloadEventExport

// ExportPath is /events/{event_id}/exports/{export_id}.
type ExportPath struct {
	EventPath
	ExportID string `path:"export_id" doc:"Export ID"`
}

func (p ExportPath) exportID() uuid.UUID { return httpx.ParseID(p.ExportID) }

type exportURLOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ExportURLDTO
}

func (h handler) downloadExport(ctx context.Context, in *ExportPath) (*exportURLOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	dl, err := h.svc.DownloadExport(ctx, a, in.id(), in.exportID())
	if err != nil {
		return nil, err
	}
	return &exportURLOutput{CacheControl: "no-store", Body: ExportURLDTO(dl)}, nil
}

func exportFields(in exportFieldList) []string {
	out := make([]string, len(in))
	for i, f := range in {
		out[i] = string(f)
	}
	return out
}
