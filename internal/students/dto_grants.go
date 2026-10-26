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

// StudentAccessGrantDTO is one company I gave level 3 access
// (listMyAccessGrants): active, or suspended since my student verification
// lapsed (BE-077).
type StudentAccessGrantDTO struct {
	GrantID     string                 `json:"grant_id" format:"uuid"`
	Company     StudentGrantCompanyDTO `json:"company"`
	Source      string                 `json:"source" enum:"contact_request,application" doc:"What you accepted: a contact request (an application in phase 3)"`
	GrantedAt   time.Time              `json:"granted_at"`
	Status      string                 `json:"status" enum:"active,suspended" doc:"active: the company sees your profile; suspended: your student verification lapsed, the company sees nothing and the grant never reopens by itself (revoke it, resume it before resumable_until once verified again, or accept a new contact request of the company)"`
	SuspendedAt *time.Time             `json:"suspended_at,omitempty" doc:"When the lapse of your verification suspended the grant (suspended grants only)"`
	// ResumableUntil is the end of the suspension window (BE-088).
	ResumableUntil *time.Time `json:"resumable_until,omitempty" doc:"Until when you may resume the suspended grant once verified again; after it the grant ends (suspended grants only)"`
	// ContactSharedAt: BE-089 (T-345), per-company contact sharing.
	ContactSharedAt *time.Time `json:"contact_shared_at,omitempty" doc:"Since when this company sees your e-mail (you shared it when you accepted its contact request); absent: not shared. unshareMyAccessGrantContact stops it"`
}

// StudentAccessGrantList is a page of listMyAccessGrants.
type StudentAccessGrantList struct {
	Items      []StudentAccessGrantDTO `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// StudentL3CardList is a page of queryCompanyGrantedStudents.
type StudentL3CardList struct {
	Items      []StudentL3CardDTO `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func toAccessGrantDTO(v GrantView) StudentAccessGrantDTO {
	d := StudentAccessGrantDTO{
		GrantID:   v.Grant.ID.String(),
		Company:   StudentGrantCompanyDTO{OrgID: v.Grant.CompanyOrgID.String(), Name: v.CompanyName},
		Source:    string(v.Grant.Source),
		GrantedAt: v.Grant.GrantedAt.UTC(),
		Status:    string(v.Grant.Status()),
	}
	if at := v.Grant.SuspendedAt; at != nil {
		u := at.UTC()
		d.SuspendedAt = &u
	}
	if at := v.ResumableUntil; at != nil {
		u := at.UTC()
		d.ResumableUntil = &u
	}
	if at := v.Grant.ContactSharedAt; at != nil {
		u := at.UTC()
		d.ContactSharedAt = &u
	}
	return d
}
