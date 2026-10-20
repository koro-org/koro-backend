package students

import "time"

// Level 3 grant lists (04 §2.4; BE-046). The student's list names the
// companies that hold access; the company's list is level 3 cards
// (StudentL3CardDTO, dto.go). Neither carries a revocation reason: a
// reason is never shown to the other side.

// StudentGrantCompanyDTO is the company of one of my grants. logo_url is
// not in orgs.OrgSummary yet (T-251, as feed's org cards).
type StudentGrantCompanyDTO struct {
	OrgID string `json:"org_id" format:"uuid"`
	Name  string `json:"name"`
}

// StudentAccessGrantDTO is one company that sees my level 3 profile
// (listMyAccessGrants).
type StudentAccessGrantDTO struct {
	GrantID   string                 `json:"grant_id" format:"uuid"`
	Company   StudentGrantCompanyDTO `json:"company"`
	Source    string                 `json:"source" enum:"contact_request,application" doc:"What you accepted: a contact request (an application in phase 3)"`
	GrantedAt time.Time              `json:"granted_at"`
}

// StudentAccessGrantList is a page of listMyAccessGrants.
type StudentAccessGrantList struct {
	Items      []StudentAccessGrantDTO `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// StudentL3CardList is a page of listCompanyGrantedStudents.
type StudentL3CardList struct {
	Items      []StudentL3CardDTO `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func toAccessGrantDTO(v GrantView) StudentAccessGrantDTO {
	return StudentAccessGrantDTO{
		GrantID:   v.Grant.ID.String(),
		Company:   StudentGrantCompanyDTO{OrgID: v.Grant.CompanyOrgID.String(), Name: v.CompanyName},
		Source:    string(v.Grant.Source),
		GrantedAt: v.Grant.GrantedAt.UTC(),
	}
}
