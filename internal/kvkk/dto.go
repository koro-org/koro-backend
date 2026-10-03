package kvkk

import (
	"time"
	"uuid"

	"koro/internal/platform/page"
)

// DsrSelfDTO is a request as its data subject sees it (04 §2.22).
type DsrSelfDTO struct {
	DSRID      string    `json:"dsr_id" format:"uuid"`
	Type       string    `json:"type" enum:"access,export,rectification,erasure,restriction,objection,profiling_access,profiling_objection,info"`
	Status     string    `json:"status" enum:"received,identity_check,in_progress,awaiting_second_approval,completed,rejected,cancelled"`
	ReceivedAt time.Time `json:"received_at"`
	DueAt      time.Time `json:"due_at" doc:"Legal answer deadline: received_at + 30 days (KVKK md. 13)"`
}

func toSelfDTO(d DSR) DsrSelfDTO {
	return DsrSelfDTO{DSRID: d.ID.String(), Type: string(d.Type), Status: string(d.Status), ReceivedAt: d.ReceivedAt, DueAt: d.DueAt}
}

// DsrSelfDetailDTO is getMyDataRequest: DsrSelfDTO + the answer.
type DsrSelfDetailDTO struct {
	DsrSelfDTO
	ResponseSummary string `json:"response_summary,omitempty" doc:"The DPO's answer (and, after an erasure, its report)"`
	RejectionReason string `json:"rejection_reason,omitempty" doc:"Reason of a rejected request (KVKK md. 13)"`
}

// DsrSelfList is the body of listMyDataRequests.
type DsrSelfList struct {
	Items      []DsrSelfDTO `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// DsrAdminDTO is a request on the DPO queue (04 §2.22).
type DsrAdminDTO struct {
	DSRID              string     `json:"dsr_id" format:"uuid"`
	UserID             string     `json:"user_id,omitempty" format:"uuid" doc:"Absent for an applicant without an account"`
	RequesterName      string     `json:"requester_name,omitempty"`
	RequesterEmail     string     `json:"requester_email,omitempty"`
	Type               string     `json:"type" enum:"access,export,rectification,erasure,restriction,objection,profiling_access,profiling_objection,info"`
	Channel            string     `json:"channel" enum:"in_app,email,post,kep,in_person"`
	Status             string     `json:"status" enum:"received,identity_check,in_progress,awaiting_second_approval,completed,rejected,cancelled"`
	Details            string     `json:"details,omitempty"`
	ReceivedAt         time.Time  `json:"received_at"`
	DueAt              time.Time  `json:"due_at"`
	DaysLeft           int        `json:"days_left" doc:"Whole days until due_at (≤ 0: overdue)"`
	IdentityVerifiedAt *time.Time `json:"identity_verified_at,omitempty"`
	AssignedTo         string     `json:"assigned_to,omitempty" format:"uuid"`
	FirstApprover      string     `json:"first_approver,omitempty" format:"uuid"`
	SecondApprover     string     `json:"second_approver,omitempty" format:"uuid"`
	ResponseSummary    string     `json:"response_summary,omitempty"`
	ExportAvailable    bool       `json:"export_available"`
	RejectionReason    string     `json:"rejection_reason,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func toAdminDTO(d DSR, now time.Time) DsrAdminDTO {
	return DsrAdminDTO{
		DSRID: d.ID.String(), UserID: idText(d.UserID), RequesterName: d.RequesterName, RequesterEmail: d.RequesterEmail,
		Type: string(d.Type), Channel: string(d.Channel), Status: string(d.Status), Details: d.Details,
		ReceivedAt: d.ReceivedAt, DueAt: d.DueAt, DaysLeft: d.DaysLeft(now), IdentityVerifiedAt: d.IdentityVerifiedAt,
		AssignedTo: idText(d.AssignedTo), FirstApprover: idText(d.FirstApprover), SecondApprover: idText(d.SecondApprover),
		ResponseSummary: d.ResponseSummary, ExportAvailable: d.ExportFileID != nil, RejectionReason: d.RejectionReason,
		CompletedAt: d.CompletedAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

// DsrAdminList is the body of adminListDataRequests.
type DsrAdminList struct {
	Items      []DsrAdminDTO `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// SignedURLDTO is a presigned download (04 §1.11: ≤ 5 minutes).
type SignedURLDTO struct {
	URL       string    `json:"url" format:"uri"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DsrReauthDTO is the reauth body member of an R operation (04 §1.11). It is
// never logged; the audit row records only its kind.
type DsrReauthDTO struct {
	Password string `json:"password,omitempty" maxLength:"1024" doc:"Account password (account without MFA)"`
	TOTPCode string `json:"totp_code,omitempty" pattern:"^[0-9]{6}$" doc:"Current TOTP code (account with MFA; recovery codes are not accepted)"`
}

func (r *DsrReauthDTO) proof() ReauthProof {
	if r == nil {
		return ReauthProof{}
	}
	return ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

// Cursor sort keys.
const (
	sortOwn   = "received_at_desc"
	sortAdmin = "due_at_asc"
)

func cursorOf(sort, fp string, n *After) string {
	if n == nil {
		return ""
	}
	return page.Next(sort, fp, page.TimeVal(n.At), page.UUIDVal(n.ID))
}

func afterOf(cursor, sort, fp string) (*After, error) {
	c, err := page.Decode(cursor, sort, fp, 2)
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
