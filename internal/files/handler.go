package files

import (
	"context"
	"encoding/hex"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Register adds the files operations (04 §2.20) to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "createUpload", Method: http.MethodPost, Path: "/files/uploads",
		Summary: "Start a file upload", Tags: []string{"files"}, DefaultStatus: http.StatusCreated,
		Description: "Checks the purpose's MIME/size allow-list and returns a presigned PUT URL " +
			"(valid 10 minutes) for the object storage. Send exactly upload_headers with the PUT; " +
			"the API never receives the file itself (04 §1.14).",
	}, op.Meta{
		Action: string(authz.FilesUploadCreate), Surface: op.SurfaceSelf,
		RateLimit: op.RateUpload, Idempotency: op.IdempotencyOptional,
	}), h.createUpload)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "completeUpload", Method: http.MethodPost, Path: "/files/{file_id}/complete",
		Summary: "Complete a file upload", Tags: []string{"files"},
		Description: "Verifies the uploaded object (size, SHA-256, magic bytes; 422 on a mismatch) " +
			"and queues the virus scan. Repeating it with the same sha256 returns the current state.",
	}, op.Meta{
		Action: string(authz.FilesUploadComplete), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional,
	}), h.completeUpload)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "deleteMyFile", Method: http.MethodDelete, Path: "/files/{file_id}",
		Summary: "Delete my unattached file", Tags: []string{"files"}, DefaultStatus: http.StatusNoContent,
		Description: "Deletes a file the caller uploaded and no record uses yet; an attached file " +
			"is deleted where it is used (409).",
	}, op.Meta{Action: string(authz.FilesFileDeleteOwn), Surface: op.SurfaceSelf}), h.deleteMyFile)
}

type handler struct{ svc Service }

// ---- createUpload

type createUploadInput struct {
	idempotency.OptionalHeader
	Body struct {
		Purpose     string `json:"purpose" enum:"avatar,cv,edu_document,org_logo,event_cover,post_media,club_claim_evidence,party_document,bank_account_evidence,transfer_receipt,payout_receipt,refund_receipt,invoice_pdf,delivery_evidence" doc:"What the file is for; decides the MIME/size allow-list and who may upload (03 k30)"`
		Filename    string `json:"filename" minLength:"1" maxLength:"1024" doc:"Original file name (display only; never part of the storage key)"`
		ContentType string `json:"content_type" enum:"application/pdf,image/jpeg,image/png,image/webp" doc:"Declared MIME type; verified by magic bytes on completion"`
		SizeBytes   int64  `json:"size_bytes" minimum:"1" maximum:"10485760" doc:"Exact size in bytes (the purpose may allow less: avatar 5 MB)"`
		OrgID       string `json:"org_id,omitempty" format:"uuid" doc:"Owning organisation (required for org_logo, event_cover, post_media)"`
	}
}

type fileUpload struct {
	FileID        string            `json:"file_id" format:"uuid"`
	UploadURL     string            `json:"upload_url" doc:"Presigned PUT URL of the object storage"`
	UploadHeaders map[string]string `json:"upload_headers" doc:"Headers the PUT must carry exactly (Content-Type)"`
	ExpiresAt     time.Time         `json:"expires_at"`
}

type createUploadOutput struct {
	Body fileUpload
}

func (h handler) createUpload(ctx context.Context, in *createUploadInput) (*createUploadOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	req := UploadRequest{
		Purpose: Purpose(in.Body.Purpose), Filename: in.Body.Filename, ContentType: in.Body.ContentType,
		SizeBytes: in.Body.SizeBytes,
	}
	if in.Body.OrgID != "" {
		id, err := uuid.Parse(in.Body.OrgID)
		if err != nil {
			return nil, problem.ErrValidation.WithFields(problem.FieldError{Location: "body.org_id", Message: "must be a UUID"})
		}
		req.OrgID = &id
	}
	up, err := h.svc.CreateUpload(ctx, a, req)
	if err != nil {
		return nil, err
	}
	return &createUploadOutput{Body: fileUpload{
		FileID: up.FileID.String(), UploadURL: up.UploadURL, UploadHeaders: up.UploadHeaders, ExpiresAt: up.ExpiresAt,
	}}, nil
}

// ---- completeUpload

type completeUploadInput struct {
	idempotency.OptionalHeader
	FileID string `path:"file_id" doc:"File ID (an unknown or malformed ID is 404)"`
	Body   struct {
		SHA256 string `json:"sha256" pattern:"^[0-9a-f]{64}$" doc:"Lower-case hex SHA-256 of the uploaded bytes"`
	}
}

type fileState struct {
	FileID     string `json:"file_id" format:"uuid"`
	Status     string `json:"status" enum:"uploading,active"`
	ScanStatus string `json:"scan_status" enum:"pending,clean,infected,skipped"`
}

type completeUploadOutput struct {
	Body fileState
}

func (h handler) completeUpload(ctx context.Context, in *completeUploadInput) (*completeUploadOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.FileID)
	if err != nil {
		return nil, ErrNotFound // IDOR rule: an unknown ID is simply not found
	}
	sum, err := hex.DecodeString(in.Body.SHA256)
	if err != nil {
		return nil, problem.ErrValidation.WithFields(problem.FieldError{Location: "body.sha256", Message: "must be hex"})
	}
	m, err := h.svc.CompleteUpload(ctx, a, id, sum)
	if err != nil {
		return nil, err
	}
	return &completeUploadOutput{Body: fileState{
		FileID: m.ID.String(), Status: string(m.Status), ScanStatus: string(m.ScanStatus),
	}}, nil
}

// ---- deleteMyFile

type deleteMyFileInput struct {
	FileID string `path:"file_id" doc:"File ID (an unknown or malformed ID is 404)"`
}

func (h handler) deleteMyFile(ctx context.Context, in *deleteMyFileInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.FileID)
	if err != nil {
		return nil, ErrNotFound
	}
	if err := h.svc.DeleteOwn(ctx, a, id); err != nil {
		return nil, err
	}
	return nil, nil
}
