package identity

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// registerEdu adds the student verification operations (04 §2.1; BE-018).
func registerEdu(api huma.API, h handler) {
	op.Register(api, "identity", huma.Operation{
		OperationID: "startEduVerification", Method: http.MethodPost, Path: "/me/edu-verifications", DefaultStatus: http.StatusAccepted,
		Summary: "Verify (or renew) my student status with a .edu.tr address",
		Description: "A single-use link (valid 24 hours) goes to the address. 202 whether or not the address is " +
			"free: an address that verifies another account gets nothing (no enumeration). An address whose domain " +
			"is not a known university is 422 identity.edu_domain_unknown: submit a student document instead.",
	}, op.Meta{Action: string(authz.IdentityEduVerificationStart), Surface: op.SurfaceSelf, RateLimit: op.RateAuthEmail,
		Idempotency: op.IdempotencyOptional}, h.startEdu)

	op.Register(api, "identity", huma.Operation{
		OperationID: "confirmEduVerification", Method: http.MethodPost, Path: "/auth/edu-verifications/confirm",
		DefaultStatus: http.StatusNoContent, Summary: "Confirm a .edu.tr address",
		Description: "Single use; an unknown, used or expired token is 422 identity.invalid_token. The account becomes a " +
			"verified student for one year; a renewal supersedes the previous verification. An address that " +
			"verifies another account is 409 identity.edu_email_taken.",
	}, op.Meta{Action: string(authz.IdentityEduVerificationConfirm), Surface: op.SurfacePublic, RateLimit: op.RateTokenConfirm,
		Idempotency: op.IdempotencyOptional}, h.confirmEdu)

	op.Register(api, "identity", huma.Operation{
		OperationID: "submitEduDocument", Method: http.MethodPost, Path: "/me/edu-verifications/document",
		DefaultStatus: http.StatusAccepted, Summary: "Submit a student document for review",
		Description: "For students without a .edu.tr address (open education). document_file_id is a completed " +
			"edu_document upload of the caller (createUpload). The document waits for platform staff; one at a " +
			"time (409 identity.edu_verification_pending).",
	}, op.Meta{Action: string(authz.IdentityEduVerificationSubmitDocument), Surface: op.SurfaceSelf,
		Idempotency: op.IdempotencyOptional}, h.submitEduDocument)

	op.Register(api, "identity", huma.Operation{
		OperationID: "getMyEduVerification", Method: http.MethodGet, Path: "/me/edu-verification",
		Summary: "My student verification", Description: "The .edu.tr address is masked (a***@ogr.x.edu.tr).",
	}, op.Meta{Action: string(authz.IdentityEduVerificationReadOwn), Surface: op.SurfaceSelf}, h.myEdu)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminListEduVerifications", Method: http.MethodGet, Path: "/admin/edu-verifications",
		Summary: "Student document review queue",
		Description: "Student documents of one status (default pending, oldest first; other statuses newest first). " +
			"Applicants by name only; open a document with adminGetEduDocument.",
	}, op.Meta{Action: string(authz.IdentityEduVerificationList), Surface: op.SurfaceAdmin}, h.listEdu)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminGetEduDocument", Method: http.MethodGet, Path: "/admin/edu-verifications/{verification_id}/document",
		Summary: "Download a student document",
		Description: "A signed URL valid for 5 minutes; every call is audited. 409 files.not_clean while the virus " +
			"scan is pending or found something. Not for the reviewer's own document (403).",
	}, op.Meta{Action: string(authz.IdentityEduVerificationReview), Surface: op.SurfaceAdmin}, h.getEduDocument)

	op.Register(api, "identity", huma.Operation{
		OperationID: "adminDecideEduVerification", Method: http.MethodPost, Path: "/admin/edu-verifications/{verification_id}/decision",
		Summary: "Approve or reject a student document",
		Description: "verified needs university_id and a clean document; the student is verified for one year (a " +
			"current verification is superseded). rejected keeps review_note for the student. Own document: 403; " +
			"a decided verification: 409 identity.edu_invalid_state.",
	}, op.Meta{Action: string(authz.IdentityEduVerificationReview), Surface: op.SurfaceAdmin,
		Idempotency: op.IdempotencyOptional}, h.decideEdu)
}

// ---- DTOs

// UniversityRefDTO names a university (the name comes from directory when
// it is wired).
type UniversityRefDTO struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name,omitempty"`
}

// EduVerificationDTO is one student verification of the caller.
type EduVerificationDTO struct {
	VerificationID string            `json:"verification_id" format:"uuid"`
	Method         string            `json:"method" enum:"edu_email,document"`
	Status         string            `json:"status" enum:"pending,verified,rejected,expired,revoked,superseded"`
	University     *UniversityRefDTO `json:"university,omitempty"`
	EduEmail       string            `json:"edu_email,omitempty" doc:"Masked: a***@ogr.x.edu.tr"`
	SubmittedAt    time.Time         `json:"submitted_at"`
	VerifiedAt     *time.Time        `json:"verified_at,omitempty"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	ReviewNote     string            `json:"review_note,omitempty" doc:"The reviewer's note on a rejected document"`
}

// MyEduVerificationDTO is getMyEduVerification (04 §2.1).
type MyEduVerificationDTO struct {
	Status          string              `json:"status" enum:"none,pending,verified,lapsed,rejected"`
	VerificationID  string              `json:"verification_id,omitempty" format:"uuid"`
	Method          string              `json:"method,omitempty" enum:"edu_email,document"`
	University      *UniversityRefDTO   `json:"university,omitempty"`
	EduEmail        string              `json:"edu_email,omitempty" doc:"Masked: a***@ogr.x.edu.tr"`
	VerifiedAt      *time.Time          `json:"verified_at,omitempty"`
	ExpiresAt       *time.Time          `json:"expires_at,omitempty"`
	ReviewNote      string              `json:"review_note,omitempty" doc:"The reviewer's note when status is rejected"`
	PendingDocument *EduVerificationDTO `json:"pending_document,omitempty" doc:"A document in review next to a current verification (renewal)"`
}

// AdminEduUserDTO is the applicant of a queue item.
type AdminEduUserDTO struct {
	ID        string `json:"id" format:"uuid"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// AdminEduVerificationDTO is one item of the review queue (04 §2.1).
type AdminEduVerificationDTO struct {
	VerificationID string            `json:"verification_id" format:"uuid"`
	User           AdminEduUserDTO   `json:"user"`
	Method         string            `json:"method" enum:"edu_email,document"`
	Status         string            `json:"status" enum:"pending,verified,rejected,expired,revoked,superseded"`
	University     *UniversityRefDTO `json:"university,omitempty" doc:"Claimed while pending, confirmed once verified"`
	CreatedAt      time.Time         `json:"created_at"`
	VerifiedAt     *time.Time        `json:"verified_at,omitempty"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	ReviewedBy     string            `json:"reviewed_by,omitempty" format:"uuid"`
	ReviewNote     string            `json:"review_note,omitempty"`
}

// AdminEduVerificationList is the body of adminListEduVerifications.
type AdminEduVerificationList struct {
	Items      []AdminEduVerificationDTO `json:"items"`
	NextCursor string                    `json:"next_cursor,omitempty"`
}

// EduDocumentDTO is the signed download URL of a student document.
type EduDocumentDTO struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func universityRef(v EduView) *UniversityRefDTO {
	if v.Record.UniversityID == nil {
		return nil
	}
	return &UniversityRefDTO{ID: v.Record.UniversityID.String(), Name: v.UniversityName}
}

func toEduDTO(v EduView) EduVerificationDTO {
	r := v.Record
	d := EduVerificationDTO{
		VerificationID: r.ID.String(), Method: string(r.Method), Status: string(r.Status), University: universityRef(v),
		EduEmail: maskEduEmail(r.EduEmail), SubmittedAt: r.CreatedAt, VerifiedAt: r.VerifiedAt, ExpiresAt: r.ExpiresAt,
	}
	if r.Status == EduRejected {
		d.ReviewNote = r.ReviewNote
	}
	return d
}

func toMyEduDTO(m MyEdu) MyEduVerificationDTO {
	d := MyEduVerificationDTO{Status: m.Status}
	if c := m.Current; c != nil {
		e := toEduDTO(*c)
		d.VerificationID, d.Method, d.University, d.EduEmail = e.VerificationID, e.Method, e.University, e.EduEmail
		d.VerifiedAt, d.ExpiresAt, d.ReviewNote = e.VerifiedAt, e.ExpiresAt, e.ReviewNote
	}
	if p := m.PendingDocument; p != nil {
		e := toEduDTO(*p)
		d.PendingDocument = &e
	}
	return d
}

func toAdminEduDTO(v EduView) AdminEduVerificationDTO {
	r := v.Record
	d := AdminEduVerificationDTO{
		VerificationID: r.ID.String(), User: AdminEduUserDTO{ID: r.UserID.String()}, Method: string(r.Method),
		Status: string(r.Status), University: universityRef(v), CreatedAt: r.CreatedAt, VerifiedAt: r.VerifiedAt,
		ExpiresAt: r.ExpiresAt, ReviewNote: r.ReviewNote,
	}
	if u := v.User; u != nil {
		d.User.FirstName, d.User.LastName = u.FirstName, u.LastName
	}
	if r.ReviewedBy != nil {
		d.ReviewedBy = r.ReviewedBy.String()
	}
	return d
}

// ---- handlers

type startEduInput struct {
	Body struct {
		EduEmail string `json:"edu_email" maxLength:"320" doc:"A .edu.tr address of the caller's university"`
	}
}

func (h handler) startEdu(ctx context.Context, in *startEduInput) (*acceptedOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.StartEduVerification(ctx, a, in.Body.EduEmail); err != nil {
		return nil, err
	}
	return &acceptedOutput{}, nil
}

func (h handler) confirmEdu(ctx context.Context, in *tokenInput) (*struct{}, error) {
	return nil, h.svc.ConfirmEduVerification(ctx, authz.ActorOrAnonymous(ctx), in.Body.Token)
}

type submitEduDocumentInput struct {
	Body struct {
		UniversityID   string `json:"university_id" format:"uuid" doc:"The university the student attends"`
		DocumentFileID string `json:"document_file_id" format:"uuid" doc:"A completed edu_document upload of the caller"`
	}
}

type eduOutput struct {
	Body EduVerificationDTO
}

func (h handler) submitEduDocument(ctx context.Context, in *submitEduDocumentInput) (*eduOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	uni, err := uuid.Parse(in.Body.UniversityID)
	if err != nil {
		return nil, problem.Field("body.university_id", "must be a UUID")
	}
	file, err := uuid.Parse(in.Body.DocumentFileID)
	if err != nil {
		return nil, problem.Field("body.document_file_id", "must be a UUID")
	}
	v, err := h.svc.SubmitEduDocument(ctx, a, EduDocumentSubmission{UniversityID: uni, FileID: file})
	if err != nil {
		return nil, err
	}
	return &eduOutput{Body: toEduDTO(v)}, nil
}

type myEduOutput struct {
	Body MyEduVerificationDTO
}

func (h handler) myEdu(ctx context.Context, _ *struct{}) (*myEduOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.MyEduVerification(ctx, a)
	if err != nil {
		return nil, err
	}
	return &myEduOutput{Body: toMyEduDTO(m)}, nil
}

type listEduInput struct {
	Status string `query:"status" enum:"pending,verified,rejected,expired,revoked,superseded" doc:"Default pending"`
	page.Params
}

type listEduOutput struct {
	Body AdminEduVerificationList
}

const eduSort = "id"

func (h handler) listEdu(ctx context.Context, in *listEduInput) (*listEduOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListEduVerifications", in.Status)
	c, err := page.Decode(in.Cursor, eduSort, fp, 1)
	if err != nil {
		return nil, err
	}
	q := EduQuery{Status: EduStatus(in.Status), Limit: in.Limit}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		q.After = &id
	}
	p, err := h.svc.EduQueue(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &listEduOutput{Body: AdminEduVerificationList{Items: make([]AdminEduVerificationDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toAdminEduDTO(v)
	}
	if p.More && len(p.Items) > 0 {
		out.Body.NextCursor = page.Next(eduSort, fp, page.UUIDVal(p.Items[len(p.Items)-1].Record.ID))
	}
	return out, nil
}

type verificationIDInput struct {
	VerificationID string `path:"verification_id" doc:"Verification ID (an unknown or malformed ID is 404)"`
}

// eduPathID parses a verification ID; a malformed one is decided like an
// unknown one (the gate first: a caller without the action gets 403).
func eduPathID(ctx context.Context, a authz.Actor, raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		if gate := authz.Can(ctx, a, authz.IdentityEduVerificationReview, eduGate(uuid.Max())); gate != nil {
			return uuid.Nil(), gate
		}
		return uuid.Nil(), ErrNotFound
	}
	return id, nil
}

type eduDocumentOutput struct {
	Body EduDocumentDTO
}

func (h handler) getEduDocument(ctx context.Context, in *verificationIDInput) (*eduDocumentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := eduPathID(ctx, a, in.VerificationID)
	if err != nil {
		return nil, err
	}
	l, err := h.svc.EduDocument(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &eduDocumentOutput{Body: EduDocumentDTO(l)}, nil
}

type decideEduInput struct {
	VerificationID string `path:"verification_id" doc:"Verification ID (an unknown or malformed ID is 404)"`
	Body           struct {
		Decision     string `json:"decision" enum:"verified,rejected"`
		UniversityID string `json:"university_id,omitempty" format:"uuid" doc:"Required to approve"`
		ReviewNote   string `json:"review_note,omitempty" maxLength:"2000" doc:"At most 500 characters; shown to the student on a rejection"`
	}
}

type adminEduOutput struct {
	Body AdminEduVerificationDTO
}

func (h handler) decideEdu(ctx context.Context, in *decideEduInput) (*adminEduOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := eduPathID(ctx, a, in.VerificationID)
	if err != nil {
		return nil, err
	}
	d := EduDecision{Decision: EduStatus(in.Body.Decision), ReviewNote: in.Body.ReviewNote}
	if in.Body.UniversityID != "" {
		uni, err := uuid.Parse(in.Body.UniversityID)
		if err != nil {
			return nil, problem.Field("body.university_id", "must be a UUID")
		}
		d.UniversityID = &uni
	}
	v, err := h.svc.DecideEduVerification(ctx, a, id, d)
	if err != nil {
		return nil, err
	}
	return &adminEduOutput{Body: toAdminEduDTO(v)}, nil
}
