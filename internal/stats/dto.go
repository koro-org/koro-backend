package stats

import (
	"time"

	"koro/internal/students"
)

// Estimate is a count about people the panel's org does not see one by
// one (06 H-61; students.AudienceCountDTO: null below 10, rounded down to
// its bucket from 10 on).
type Estimate = students.AudienceCountDTO

// RangeDTO is the period of a panel: whole days in Europe/Istanbul.
type RangeDTO struct {
	From string `json:"from" format:"date" doc:"First day (Europe/Istanbul), inclusive"`
	To   string `json:"to" format:"date" doc:"Last day (Europe/Istanbul), inclusive"`
}

// KeyCount is one row of an exact breakdown.
type KeyCount struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// KeyEstimate is one row of a breakdown about people: groups below 10 are
// left out, the others rounded down to their bucket.
type KeyEstimate struct {
	Key   string `json:"key"`
	Count int    `json:"count" doc:"Rounded down to its bucket (10 below 100, 50 below 1000, 100 below 10000, then 1000)"`
}

// ---- platform (getAdminStats)

// PlatformStats is the platform admin's summary (SR §8 Admin; §16-6).
type PlatformStats struct {
	Range   RangeDTO           `json:"range"`
	Users   PlatformUsersDTO   `json:"users"`
	Orgs    PlatformOrgsDTO    `json:"orgs"`
	Content PlatformContentDTO `json:"content"`
	Events  PlatformEventsDTO  `json:"events"`
	Queues  PlatformQueuesDTO  `json:"queues"`
}

// PlatformUsersDTO counts the accounts.
type PlatformUsersDTO struct {
	Total            int        `json:"total" doc:"Every account, tombstones (erased) included"`
	ByStatus         []KeyCount `json:"by_status" doc:"pending_email, active, suspended, erased"`
	ByPersona        []KeyCount `json:"by_persona" doc:"Accounts that are not erased, by the role chosen at sign-up"`
	EmailVerified    int        `json:"email_verified" doc:"Accounts that are not erased with a verified e-mail address"`
	VerifiedStudents int        `json:"verified_students" doc:"Users with a student verification in effect now"`
	NewInRange       int        `json:"new_in_range" doc:"Accounts created in the range"`
}

// PlatformOrgsDTO counts the orgs by kind.
type PlatformOrgsDTO struct {
	ByKind     []OrgKindDTO `json:"by_kind"`
	NewInRange int          `json:"new_in_range" doc:"Orgs created in the range"`
}

// OrgKindDTO is the orgs of one kind by status.
type OrgKindDTO struct {
	Kind       string     `json:"kind" enum:"platform,university,club,company,ngo"`
	Total      int        `json:"total"`
	ByStatus   []KeyCount `json:"by_status" doc:"pending_review, active, suspended, closed"`
	NewInRange int        `json:"new_in_range"`
}

// PlatformContentDTO counts the posts and content reports.
type PlatformContentDTO struct {
	PostsByStatus    []KeyCount `json:"posts_by_status" doc:"draft, pending_review, published, hidden, removed"`
	PublishedInRange int        `json:"published_in_range" doc:"Posts first published in the range, whatever their status now"`
	ReportsByStatus  []KeyCount `json:"reports_by_status" doc:"open, in_review, actioned, dismissed"`
	ReportsInRange   int        `json:"reports_in_range" doc:"Content reports filed in the range"`
}

// PlatformEventsDTO counts the events.
type PlatformEventsDTO struct {
	Upcoming         int      `json:"upcoming" doc:"Published events that start after now"`
	HeldInRange      int      `json:"held_in_range" doc:"Published or completed events that started in the range by now"`
	CancelledInRange int      `json:"cancelled_in_range" doc:"Cancelled events that were to start in the range"`
	Registered       int      `json:"registered" doc:"Places kept (confirmed, checked in, no-show) in the events held in the range"`
	CheckedIn        int      `json:"checked_in" doc:"Check-ins of those places"`
	AttendanceRate   *float64 `json:"attendance_rate" doc:"checked_in / registered (0–1); null without a registration"`
}

// PlatformQueuesDTO is the size of the review queues.
type PlatformQueuesDTO struct {
	OrgReviews     int `json:"org_reviews" doc:"Orgs awaiting verification (pending_review)"`
	ClubClaims     int `json:"club_claims" doc:"Pending club claims, every university"`
	ContentReports int `json:"content_reports" doc:"Content reports open or in review"`
}

// ---- club (getClubStats)

// ClubStats is a club's panel (SR §8 Kulüp).
type ClubStats struct {
	Range       RangeDTO           `json:"range"`
	Members     ClubMembersDTO     `json:"members"`
	Events      ClubEventsDTO      `json:"events"`
	Sponsorship ClubSponsorshipDTO `json:"sponsorship"`
}

// ClubMembersDTO is the club's members and their growth.
type ClubMembersDTO struct {
	Active          int        `json:"active" doc:"Active members now (every role)"`
	PendingRequests int        `json:"pending_requests" doc:"Join requests awaiting a decision now"`
	JoinedInRange   int        `json:"joined_in_range" doc:"Memberships that started in the range (a role change is not a join)"`
	LeftInRange     int        `json:"left_in_range" doc:"Memberships that ended in the range (a role change is not a departure)"`
	NetGrowth       int        `json:"net_growth" doc:"joined_in_range − left_in_range"`
	Monthly         []MonthDTO `json:"monthly" doc:"Every calendar month of the range (Europe/Istanbul), oldest first"`
}

// MonthDTO is one month of the membership growth.
type MonthDTO struct {
	Month  string `json:"month" doc:"YYYY-MM"`
	Joined int    `json:"joined"`
	Left   int    `json:"left"`
}

// ClubEventsDTO is the club's events that start in the range.
type ClubEventsDTO struct {
	Count          int            `json:"count" doc:"Events that start in the range and were published (cancelled ones excluded)"`
	Cancelled      int            `json:"cancelled"`
	Registered     int            `json:"registered" doc:"Places kept (confirmed, checked in, no-show) in the events held by now"`
	CheckedIn      int            `json:"checked_in"`
	NoShow         int            `json:"no_show"`
	AttendanceRate *float64       `json:"attendance_rate" doc:"checked_in / registered over the events held by now (0–1); null without a registration"`
	Items          []ClubEventDTO `json:"items" doc:"The latest events of the range first, at most 50"`
	Partial        bool           `json:"partial" doc:"The range holds more than 500 events: the figures cover the latest 500"`
}

// ClubEventDTO is one event of the club's panel.
type ClubEventDTO struct {
	EventID        string    `json:"event_id" format:"uuid"`
	Title          string    `json:"title"`
	Status         string    `json:"status" enum:"published,cancelled,completed"`
	StartsAt       time.Time `json:"starts_at"`
	Held           bool      `json:"held" doc:"The event started by now and was not cancelled"`
	Registered     int       `json:"registered"`
	CheckedIn      int       `json:"checked_in"`
	NoShow         int       `json:"no_show"`
	AttendanceRate *float64  `json:"attendance_rate" doc:"checked_in / registered once held; null before or without a registration"`
}

// ClubSponsorshipDTO is the orders companies placed on the club's
// offerings in the range (sponsorship requests), each once.
type ClubSponsorshipDTO struct {
	Orders   int        `json:"orders"`
	ByStatus []KeyCount `json:"by_status" doc:"Order status now: awaiting_payment, paid, in_fulfilment, completed, cancelled, expired, partially_refunded, refunded, disputed"`
}

// ---- company (getCompanyStats)

// CompanyStats is a company's panel (SR §8 Şirket).
type CompanyStats struct {
	Range           RangeDTO             `json:"range"`
	Audience        CompanyAudienceDTO   `json:"audience"`
	Posts           CompanyPostsDTO      `json:"posts"`
	Dispatches      CompanyDispatchesDTO `json:"dispatches"`
	ContactRequests CompanyContactsDTO   `json:"contact_requests"`
	Package         []FeatureUsageDTO    `json:"package"`
}

// CompanyAudienceDTO is the company's saved audiences with their stored
// estimates (no new count: the daily count budget is not spent). Other
// filters are countAudience's.
type CompanyAudienceDTO struct {
	ActiveSegments int                  `json:"active_segments"`
	Segments       []SegmentEstimateDTO `json:"segments" doc:"Largest estimate first, at most 20"`
}

// SegmentEstimateDTO is one saved audience.
type SegmentEstimateDTO struct {
	SegmentID string     `json:"segment_id" format:"uuid"`
	Name      string     `json:"name"`
	Estimate  Estimate   `json:"estimate"`
	CountedAt *time.Time `json:"counted_at,omitempty"`
}

// CompanyPostsDTO is the reach of the posts published in the range.
type CompanyPostsDTO struct {
	Published int `json:"published"`
	Targeted  int `json:"targeted" doc:"Of which targeted (segment audience)"`
	Likes     int `json:"likes" doc:"Likes the posts hold now"`
	Saves     int `json:"saves" doc:"Saves the posts hold now"`
}

// CompanyDispatchesDTO is the notification dispatches created in the range.
type CompanyDispatchesDTO struct {
	Count    int        `json:"count"`
	ByStatus []KeyCount `json:"by_status" doc:"scheduled, running, completed, cancelled, failed"`
	Eligible Estimate   `json:"eligible" doc:"Students the dispatches addressed, as one estimate"`
	Sent     Estimate   `json:"sent" doc:"Students notified, as one estimate"`
}

// CompanyContactsDTO is the contact requests sent in the range.
type CompanyContactsDTO struct {
	Sent           int        `json:"sent"`
	ByStatus       []KeyCount `json:"by_status" doc:"Effective status: pending, accepted, declined, expired, withdrawn"`
	AcceptanceRate *float64   `json:"acceptance_rate" doc:"accepted / (accepted + declined + expired) (0–1); null before any answer"`
}

// FeatureUsageDTO is one feature of the company's package over its rights
// usable now.
type FeatureUsageDTO struct {
	Feature     string     `json:"feature" enum:"targeted_post,targeted_dispatch,contact_request,discovery_access"`
	Total       int        `json:"total" doc:"Units of the usable rights (quota features)"`
	Used        int        `json:"used"`
	Remaining   int        `json:"remaining"`
	AccessUntil *time.Time `json:"access_until,omitempty" doc:"discovery_access: end of the running right"`
}

// ---- university (getUniversityStats)

// UniversityStats is a university's panel (SR §8 Kurum / Yönetici).
type UniversityStats struct {
	Range    RangeDTO              `json:"range"`
	Students UniversityStudentsDTO `json:"students"`
	Clubs    UniversityClubsDTO    `json:"clubs"`
	Pending  UniversityPendingDTO  `json:"pending"`
}

// UniversityStudentsDTO is the university's active students, one
// breakdown at a time; a group below 10 is left out.
type UniversityStudentsDTO struct {
	Total         Estimate      `json:"total"`
	NewInRange    Estimate      `json:"new_in_range" doc:"Student profiles created in the range"`
	ByDegreeLevel []KeyEstimate `json:"by_degree_level" doc:"associate, bachelor, master, doctorate"`
	ByClassYear   []KeyEstimate `json:"by_class_year" doc:"1–8"`
	ByDepartment  []KeyEstimate `json:"by_department" doc:"Department IDs (names: the directory's departments)"`
}

// UniversityClubsDTO is the event density of the university's clubs.
// Sensitive clubs and events of a sensitive category are left out of the
// activity figures.
type UniversityClubsDTO struct {
	Total         int               `json:"total" doc:"Clubs of the university in the catalog"`
	Claimed       int               `json:"claimed" doc:"Clubs with a club_admin"`
	ActiveInRange int               `json:"active_in_range" doc:"Clubs with at least one event in the range"`
	EventsInRange int               `json:"events_in_range"`
	Participants  Estimate          `json:"participants" doc:"Places kept in the clubs' events held by now"`
	Items         []ClubActivityDTO `json:"items" doc:"The most active clubs first, at most 50"`
}

// ClubActivityDTO is one club's events of the range.
type ClubActivityDTO struct {
	OrgID        string   `json:"org_id" format:"uuid"`
	Name         string   `json:"name"`
	Events       int      `json:"events"`
	Participants Estimate `json:"participants"`
}

// UniversityPendingDTO is what waits for the university's decision now.
type UniversityPendingDTO struct {
	ClubClaims        int `json:"club_claims" doc:"Pending claims of its non-sensitive clubs, not yet escalated to the platform"`
	OfferingApprovals int `json:"offering_approvals" doc:"Offerings of its clubs that wait for its decision"`
}
