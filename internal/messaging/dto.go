package messaging

import (
	"time"

	"koro/internal/orgs"
)

// ContactRequestCompanyDTO is a request as its company sees it (04 §2.9).
// The student appears under the company's pseudonym; student_user_id (the
// level 3 profile link) only once the student accepted. A decline carries
// no reason: there is none.
type ContactRequestCompanyDTO struct {
	RequestID         string     `json:"request_id" format:"uuid"`
	AnonID            string     `json:"anon_id" doc:"The company's pseudonym of the student (as in discovery)"`
	StudentUserID     string     `json:"student_user_id,omitempty" format:"uuid" doc:"Only once accepted: the student's level 3 profile (getCompanyStudentProfile)"`
	SenderUserID      string     `json:"sender_user_id" format:"uuid" doc:"The company member who sent it"`
	Via               string     `json:"via" enum:"discovery,opportunity_post"`
	OpportunityPostID string     `json:"opportunity_post_id,omitempty" format:"uuid"`
	Subject           string     `json:"subject"`
	Message           string     `json:"message"`
	Status            string     `json:"status" enum:"pending,accepted,declined,expired,withdrawn" doc:"A pending request past expires_at reads as expired"`
	ExpiresAt         time.Time  `json:"expires_at"`
	RespondedAt       *time.Time `json:"responded_at,omitempty" doc:"When the student accepted or declined"`
	CreatedAt         time.Time  `json:"created_at"`
}

// ContactRequestCompanyList is a page of listSentContactRequests.
type ContactRequestCompanyList struct {
	Items      []ContactRequestCompanyDTO `json:"items"`
	NextCursor string                     `json:"next_cursor,omitempty"`
}

// ContactCompanyDTO is the company of a request or a block (04 §2.9
// "OrgCardDTO"): public org data only.
type ContactCompanyDTO struct {
	OrgID string `json:"org_id" format:"uuid"`
	Kind  string `json:"kind" enum:"platform,university,club,company,ngo"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
}

// ContactOpportunityPostDTO is the opportunity call a request refers to;
// the title while the post is published.
type ContactOpportunityPostDTO struct {
	PostID string `json:"post_id" format:"uuid"`
	Title  string `json:"title,omitempty"`
}

// ContactRequestStudentDTO is a request as its student sees it (04 §2.9).
type ContactRequestStudentDTO struct {
	RequestID       string                     `json:"request_id" format:"uuid"`
	Company         ContactCompanyDTO          `json:"company"`
	Subject         string                     `json:"subject"`
	Message         string                     `json:"message"`
	Status          string                     `json:"status" enum:"pending,accepted,declined,expired,withdrawn" doc:"A pending request past expires_at reads as expired"`
	ExpiresAt       time.Time                  `json:"expires_at"`
	RespondedAt     *time.Time                 `json:"responded_at,omitempty"`
	CreatedAt       time.Time                  `json:"created_at"`
	OpportunityPost *ContactOpportunityPostDTO `json:"opportunity_post,omitempty"`
}

// ContactRequestStudentList is a page of listMyContactRequests.
type ContactRequestStudentList struct {
	Items      []ContactRequestStudentDTO `json:"items"`
	NextCursor string                     `json:"next_cursor,omitempty"`
}

// CompanyBlockList is a page of listMyCompanyBlocks.
type CompanyBlockList struct {
	Items      []ContactCompanyDTO `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ToCompanyDTO is the company's view of v.
func ToCompanyDTO(v CompanyView) ContactRequestCompanyDTO {
	r := v.Request
	d := ContactRequestCompanyDTO{
		RequestID: r.ID.String(), AnonID: v.AnonID, SenderUserID: r.SenderUserID.String(), Via: string(r.Via),
		Subject: r.Subject, Message: r.Message, Status: string(v.Status), ExpiresAt: r.ExpiresAt.UTC(),
		RespondedAt: utc(r.RespondedAt), CreatedAt: r.CreatedAt.UTC(),
	}
	if v.Status == StatusAccepted {
		d.StudentUserID = r.StudentUserID.String()
	}
	if r.OpportunityPostID != nil {
		d.OpportunityPostID = r.OpportunityPostID.String()
	}
	return d
}

// companyCard is the public card of an org (the request keeps the ID when
// the org summary is missing).
func companyCard(o orgs.OrgSummary, fallbackID string) ContactCompanyDTO {
	if o.ID.String() == fallbackID {
		return ContactCompanyDTO{OrgID: fallbackID, Kind: string(o.Kind), Name: o.Name, Slug: o.Slug}
	}
	return ContactCompanyDTO{OrgID: fallbackID, Kind: string(orgs.KindCompany)}
}

// ToStudentDTO is the student's view of v.
func ToStudentDTO(v StudentView) ContactRequestStudentDTO {
	r := v.Request
	d := ContactRequestStudentDTO{
		RequestID: r.ID.String(), Company: companyCard(v.Company, r.OrgID.String()), Subject: r.Subject,
		Message: r.Message, Status: string(v.Status), ExpiresAt: r.ExpiresAt.UTC(), RespondedAt: utc(r.RespondedAt),
		CreatedAt: r.CreatedAt.UTC(),
	}
	if v.Post != nil {
		d.OpportunityPost = &ContactOpportunityPostDTO{PostID: v.Post.ID.String(), Title: v.Post.Title}
	}
	return d
}
